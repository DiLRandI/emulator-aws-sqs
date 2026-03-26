package service

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "emulator-aws-sqs/internal/errors"
	"emulator-aws-sqs/internal/protocol"
	"emulator-aws-sqs/internal/storage"

	"github.com/google/uuid"
)

const fifoDedupWindow = 5 * time.Minute

var (
	batchEntryIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
	attrNamePattern     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,256}$`)
)

type redrivePolicy struct {
	TargetARN       string
	MaxReceiveCount int64
}

func (s *SQS) SendMessage(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	now := s.clock.Now()
	if err := s.maintain(ctx, now); err != nil {
		return protocol.Response{}, err
	}

	queue, err := s.lookupQueueForRequest(ctx, req)
	if err != nil {
		return protocol.Response{}, err
	}
	if err := s.ensureQueueReady(queue, now); err != nil {
		return protocol.Response{}, err
	}

	out, err := s.sendMessageEntry(ctx, queue, req.Input, now)
	if err != nil {
		return protocol.Response{}, err
	}
	return protocol.Response{Output: out}, nil
}

func (s *SQS) SendMessageBatch(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	now := s.clock.Now()
	if err := s.maintain(ctx, now); err != nil {
		return protocol.Response{}, err
	}

	queue, err := s.lookupQueueForRequest(ctx, req)
	if err != nil {
		return protocol.Response{}, err
	}
	if err := s.ensureQueueReady(queue, now); err != nil {
		return protocol.Response{}, err
	}

	rawEntries := sliceOfMaps(anySliceValue(req.Input, "Entries"))
	if len(rawEntries) == 0 {
		return protocol.Response{}, apierrors.ErrEmptyBatchRequest
	}
	if len(rawEntries) > 10 {
		return protocol.Response{}, apierrors.ErrTooManyEntriesInBatch
	}

	seenIDs := map[string]struct{}{}
	totalBytes := 0
	for _, entry := range rawEntries {
		id := stringValue(entry, "Id")
		if !batchEntryIDPattern.MatchString(id) {
			return protocol.Response{}, apierrors.ErrInvalidBatchEntryID
		}
		if _, exists := seenIDs[id]; exists {
			return protocol.Response{}, apierrors.ErrBatchEntryIDsNotDistinct
		}
		seenIDs[id] = struct{}{}

		attrs, _, err := normalizeMessageAttributeMap(anyMapValue(entry, "MessageAttributes"), false)
		if err != nil {
			return protocol.Response{}, err
		}
		totalBytes += estimateMessageSize(stringValue(entry, "MessageBody"), attrs)
	}
	if totalBytes > 1024*1024 {
		return protocol.Response{}, apierrors.ErrBatchRequestTooLong
	}

	successful := make([]any, 0, len(rawEntries))
	failed := make([]any, 0)
	for _, entry := range rawEntries {
		id := stringValue(entry, "Id")
		out, err := s.sendMessageEntry(ctx, queue, entry, now)
		if err != nil {
			failed = append(failed, batchFailure(id, err))
			continue
		}
		out["Id"] = id
		if bodyMD5, ok := out["MD5OfMessageBody"]; ok {
			out["MD5OfMessageBody"] = bodyMD5
			delete(out, "MD5OfBody")
		}
		successful = append(successful, out)
	}

	return protocol.Response{Output: map[string]any{
		"Successful": successful,
		"Failed":     failed,
	}}, nil
}

func (s *SQS) ReceiveMessage(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	now := s.clock.Now()
	if err := s.maintain(ctx, now); err != nil {
		return protocol.Response{}, err
	}

	queue, err := s.lookupQueueForRequest(ctx, req)
	if err != nil {
		return protocol.Response{}, err
	}
	if err := s.ensureQueueReady(queue, now); err != nil {
		return protocol.Response{}, err
	}

	maxMessages, err := int64Value(req.Input, "MaxNumberOfMessages", 1)
	if err != nil || maxMessages < 1 || maxMessages > 10 {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MaxNumberOfMessages is invalid. Reason: must be between 1 and 10, if provided.")
	}
	waitSeconds, err := int64Value(req.Input, "WaitTimeSeconds", queueWaitTime(queue))
	if err != nil || waitSeconds < 0 || waitSeconds > 20 {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter WaitTimeSeconds is invalid. Reason: must be between 0 and 20, if provided.")
	}
	visibilityTimeout, err := int64Value(req.Input, "VisibilityTimeout", queueVisibilityTimeout(queue))
	if err != nil || visibilityTimeout < 0 || visibilityTimeout > 43200 {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter VisibilityTimeout is invalid. Reason: must be between 0 and 43200, if provided.")
	}

	messageAttributeNames := stringSliceValue(req.Input, "MessageAttributeNames")
	messageSystemAttributeNames := stringSliceValue(req.Input, "MessageSystemAttributeNames")
	messageSystemAttributeNames = unionStringSets(messageSystemAttributeNames, stringSliceValue(req.Input, "AttributeNames"))
	attemptID := stringValue(req.Input, "ReceiveRequestAttemptId")
	if attemptID != "" {
		if !queue.FIFO {
			return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter ReceiveRequestAttemptId is invalid. Reason: This parameter is only supported for FIFO queues.")
		}
		if len(attemptID) > 128 {
			return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter ReceiveRequestAttemptId is invalid. Reason: must be between 1 and 128 characters.")
		}
		if output, ok, err := s.replayReceiveAttempt(ctx, queue, attemptID, visibilityTimeout, messageAttributeNames, messageSystemAttributeNames, now); err != nil {
			return protocol.Response{}, err
		} else if ok {
			return protocol.Response{Output: output}, nil
		}
	}

	deadline := now.Add(time.Duration(waitSeconds) * time.Second)
	for {
		if err := s.maintain(ctx, s.clock.Now()); err != nil {
			return protocol.Response{}, err
		}

		queue, err = s.lookupQueueByURL(ctx, queue.URL)
		if err != nil {
			return protocol.Response{}, err
		}

		candidates, err := s.selectMessagesForReceive(ctx, queue, int(maxMessages), now)
		if err != nil {
			return protocol.Response{}, err
		}
		if len(candidates) > 0 {
			messages := make([]any, 0, len(candidates))
			for _, message := range candidates {
				if err := s.store.DeactivateReceiptsByMessage(ctx, message.RowID); err != nil {
					return protocol.Response{}, apierrors.Internal("failed to deactivate prior receipts", err)
				}
				leaseDeadline := s.clock.Now().Add(time.Duration(visibilityTimeout) * time.Second)
				message.ReceiveCount++
				if message.FirstReceivedAt == nil {
					first := s.clock.Now()
					message.FirstReceivedAt = &first
				}
				message.VisibilityDeadline = &leaseDeadline
				if err := s.store.UpdateMessage(ctx, message); err != nil {
					return protocol.Response{}, apierrors.Internal("failed to update message visibility", err)
				}
				receipt := storage.Receipt{
					QueueID:            queue.ID,
					MessageRowID:       message.RowID,
					Handle:             newReceiptHandle(queue.ID, message.RowID),
					IssuedAt:           s.clock.Now(),
					VisibilityDeadline: leaseDeadline,
					Active:             true,
					ReceiveAttemptID:   attemptID,
				}
				receipt, err = s.store.InsertReceipt(ctx, receipt)
				if err != nil {
					return protocol.Response{}, apierrors.Internal("failed to create receipt handle", err)
				}
				messages = append(messages, s.receiveMessageOutput(message, receipt.Handle, messageAttributeNames, messageSystemAttributeNames))
			}

			output := map[string]any{}
			if len(messages) != 0 {
				output["Messages"] = messages
			}
			if attemptID != "" && len(messages) != 0 {
				stored := make([]map[string]any, 0, len(messages))
				for _, message := range messages {
					stored = append(stored, message.(map[string]any))
				}
				_, err := s.store.InsertReceiveAttempt(ctx, storage.ReceiveAttempt{
					QueueID:   queue.ID,
					AttemptID: attemptID,
					Response:  stored,
					ExpiresAt: s.clock.Now().Add(fifoDedupWindow),
				})
				if err != nil {
					return protocol.Response{}, apierrors.Internal("failed to persist receive attempt", err)
				}
			}
			return protocol.Response{Output: output}, nil
		}

		if waitSeconds == 0 || !s.clock.Now().Before(deadline) {
			return protocol.Response{Output: map[string]any{}}, nil
		}

		sleepFor := s.cfg.LongPollWakeFrequency
		if remaining := time.Until(deadline); remaining < sleepFor {
			sleepFor = remaining
		}
		timer := s.clock.NewTimer(sleepFor)
		select {
		case <-ctx.Done():
			timer.Stop()
			return protocol.Response{}, ctx.Err()
		case <-timer.C():
		}
		now = s.clock.Now()
	}
}

func (s *SQS) DeleteMessage(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	now := s.clock.Now()
	if err := s.maintain(ctx, now); err != nil {
		return protocol.Response{}, err
	}

	queue, err := s.lookupQueueForRequest(ctx, req)
	if err != nil {
		return protocol.Response{}, err
	}
	handle := stringValue(req.Input, "ReceiptHandle")
	if err := s.deleteByReceiptHandle(ctx, queue, handle, now); err != nil {
		return protocol.Response{}, err
	}
	return protocol.Response{}, nil
}

func (s *SQS) DeleteMessageBatch(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	now := s.clock.Now()
	if err := s.maintain(ctx, now); err != nil {
		return protocol.Response{}, err
	}

	queue, err := s.lookupQueueForRequest(ctx, req)
	if err != nil {
		return protocol.Response{}, err
	}
	entries, err := validateBatchRequestEntries(anySliceValue(req.Input, "Entries"))
	if err != nil {
		return protocol.Response{}, err
	}

	successful := make([]any, 0, len(entries))
	failed := make([]any, 0)
	for _, entry := range entries {
		id := stringValue(entry, "Id")
		if err := s.deleteByReceiptHandle(ctx, queue, stringValue(entry, "ReceiptHandle"), now); err != nil {
			failed = append(failed, batchFailure(id, err))
			continue
		}
		successful = append(successful, map[string]any{"Id": id})
	}
	return protocol.Response{Output: map[string]any{
		"Successful": successful,
		"Failed":     failed,
	}}, nil
}

func (s *SQS) ChangeMessageVisibility(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	now := s.clock.Now()
	if err := s.maintain(ctx, now); err != nil {
		return protocol.Response{}, err
	}

	queue, err := s.lookupQueueForRequest(ctx, req)
	if err != nil {
		return protocol.Response{}, err
	}
	timeout, err := int64Value(req.Input, "VisibilityTimeout", 0)
	if err != nil || timeout < 0 || timeout > 43200 {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter VisibilityTimeout is invalid. Reason: must be between 0 and 43200, if provided.")
	}
	if err := s.changeVisibilityByReceiptHandle(ctx, queue, stringValue(req.Input, "ReceiptHandle"), timeout, now); err != nil {
		return protocol.Response{}, err
	}
	return protocol.Response{}, nil
}

func (s *SQS) ChangeMessageVisibilityBatch(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	now := s.clock.Now()
	if err := s.maintain(ctx, now); err != nil {
		return protocol.Response{}, err
	}

	queue, err := s.lookupQueueForRequest(ctx, req)
	if err != nil {
		return protocol.Response{}, err
	}
	entries, err := validateBatchRequestEntries(anySliceValue(req.Input, "Entries"))
	if err != nil {
		return protocol.Response{}, err
	}

	successful := make([]any, 0, len(entries))
	failed := make([]any, 0)
	for _, entry := range entries {
		id := stringValue(entry, "Id")
		timeout, err := int64Value(entry, "VisibilityTimeout", 0)
		if err != nil || timeout < 0 || timeout > 43200 {
			failed = append(failed, batchFailure(id, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter VisibilityTimeout is invalid. Reason: must be between 0 and 43200, if provided.")))
			continue
		}
		if err := s.changeVisibilityByReceiptHandle(ctx, queue, stringValue(entry, "ReceiptHandle"), timeout, now); err != nil {
			failed = append(failed, batchFailure(id, err))
			continue
		}
		successful = append(successful, map[string]any{"Id": id})
	}

	return protocol.Response{Output: map[string]any{
		"Successful": successful,
		"Failed":     failed,
	}}, nil
}

func (s *SQS) maintain(ctx context.Context, now time.Time) error {
	if err := s.store.ApplyDueQueueMutations(ctx, now); err != nil {
		return apierrors.Internal("failed to apply queue mutations", err)
	}
	if err := s.store.DeleteExpiredDedupEntries(ctx, now); err != nil {
		return apierrors.Internal("failed to reap deduplication ledger", err)
	}
	queues, err := s.store.ListQueues(ctx, s.cfg.AccountID, s.cfg.Region, "")
	if err != nil {
		return apierrors.Internal("failed to list queues for maintenance", err)
	}
	for _, queue := range queues {
		if queue.DeletedAt != nil {
			continue
		}
		if err := s.reapQueue(ctx, queue, now); err != nil {
			return err
		}
	}
	if err := s.processMoveTasks(ctx, now); err != nil {
		return err
	}
	return nil
}

func (s *SQS) reapQueue(ctx context.Context, queue storage.Queue, now time.Time) error {
	messages, err := s.store.ListMessagesByQueue(ctx, queue.ID)
	if err != nil {
		return apierrors.Internal("failed to list queue messages", err)
	}
	for _, message := range messages {
		if message.DeletedAt != nil {
			continue
		}
		if !message.RetentionDeadline.After(now) {
			message.DeletedAt = &now
			message.VisibilityDeadline = nil
			if err := s.store.UpdateMessage(ctx, message); err != nil {
				return apierrors.Internal("failed to expire message", err)
			}
			if err := s.store.DeactivateReceiptsByMessage(ctx, message.RowID); err != nil {
				return apierrors.Internal("failed to deactivate expired receipts", err)
			}
		}
	}
	if queue.PurgeRequestedAt != nil && queue.PurgeExpiresAt != nil {
		if err := s.store.DeleteMessagesByQueueBefore(ctx, queue.ID, *queue.PurgeExpiresAt); err != nil {
			return apierrors.Internal("failed to purge queue messages", err)
		}
		if !queue.PurgeExpiresAt.After(now) {
			queue.PurgeRequestedAt = nil
			queue.PurgeExpiresAt = nil
			if err := s.store.UpdateQueue(ctx, queue); err != nil {
				return apierrors.Internal("failed to clear purge window", err)
			}
		}
	}
	return nil
}

func (s *SQS) processMoveTasks(ctx context.Context, now time.Time) error {
	tasks, err := s.store.ListRunningMoveTasks(ctx)
	if err != nil {
		return apierrors.Internal("failed to list message move tasks", err)
	}
	for _, task := range tasks {
		if err := s.processMoveTask(ctx, task, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQS) processMoveTask(ctx context.Context, task storage.MoveTask, now time.Time) error {
	source, found, err := s.store.QueueByID(ctx, task.SourceQueueID)
	if err != nil {
		return apierrors.Internal("failed to load message move task source queue", err)
	}
	if !found || source.DeletedAt != nil {
		task.Status = "FAILED"
		task.FailureReason = "Source queue no longer exists."
		task.CompletedAt = &now
		task.UpdatedAt = now
		if updateErr := s.store.UpdateMoveTask(ctx, task); updateErr != nil {
			return apierrors.Internal("failed to update move task", updateErr)
		}
		return nil
	}

	sourceMessages, err := s.store.ListMessagesByQueue(ctx, source.ID)
	if err != nil {
		return apierrors.Internal("failed to list source messages for move task", err)
	}

	limit := int64(len(sourceMessages))
	if task.MaxMessagesPerSecond != nil && *task.MaxMessagesPerSecond > 0 {
		limit = *task.MaxMessagesPerSecond
	}
	remaining := int64(0)
	moved := int64(0)
	for _, message := range sourceMessages {
		if message.DeletedAt != nil || !message.RetentionDeadline.After(now) {
			continue
		}
		if moved >= limit {
			remaining++
			continue
		}
		destination, ok, err := s.resolveMoveDestination(ctx, task, message)
		if err != nil {
			return err
		}
		if !ok {
			remaining++
			continue
		}

		if err := s.store.DeactivateReceiptsByMessage(ctx, message.RowID); err != nil {
			return apierrors.Internal("failed to deactivate receipts during move task", err)
		}
		retention := now.Add(time.Duration(queueRetentionSeconds(destination)) * time.Second)
		message.QueueID = destination.ID
		message.SentAt = now
		message.OriginalSentAt = now
		message.FirstReceivedAt = nil
		message.ReceiveCount = 0
		message.AvailableAt = now
		message.VisibilityDeadline = nil
		message.RetentionDeadline = retention
		message.DeletedAt = nil
		message.DeadLetterSourceARN = ""
		if destination.FIFO {
			message.SequenceNumber = newSequenceNumber()
		} else {
			message.SequenceNumber = ""
		}
		if err := s.store.UpdateMessage(ctx, message); err != nil {
			return apierrors.Internal("failed to move message", err)
		}
		moved++
	}

	task.ApproximateNumberOfMessagesMoved += moved
	task.ApproximateNumberOfMessagesToMove = &remaining
	task.UpdatedAt = now
	if task.Status == "CANCELLING" {
		task.Status = "CANCELLED"
		task.CompletedAt = &now
	} else if remaining == 0 {
		task.Status = "COMPLETED"
		task.CompletedAt = &now
	}
	if err := s.store.UpdateMoveTask(ctx, task); err != nil {
		return apierrors.Internal("failed to update message move task", err)
	}
	return nil
}

func (s *SQS) resolveMoveDestination(ctx context.Context, task storage.MoveTask, message storage.Message) (storage.Queue, bool, error) {
	if task.DestinationQueueID != nil {
		queue, found, err := s.store.QueueByID(ctx, *task.DestinationQueueID)
		if err != nil {
			return storage.Queue{}, false, apierrors.Internal("failed to lookup move task destination queue", err)
		}
		if !found || queue.DeletedAt != nil {
			return storage.Queue{}, false, nil
		}
		return queue, true, nil
	}
	if message.DeadLetterSourceARN == "" {
		return storage.Queue{}, false, nil
	}
	queue, found, err := s.store.QueueByARN(ctx, message.DeadLetterSourceARN)
	if err != nil {
		return storage.Queue{}, false, apierrors.Internal("failed to lookup original source queue", err)
	}
	if !found || queue.DeletedAt != nil {
		return storage.Queue{}, false, nil
	}
	return queue, true, nil
}

func (s *SQS) lookupQueueForRequest(ctx context.Context, req protocol.Request) (storage.Queue, error) {
	queueURL := s.queueURLFromRequest(req)
	if queueURL == "" {
		return storage.Queue{}, apierrors.ErrQueueDoesNotExist
	}
	return s.lookupQueueByURL(ctx, queueURL)
}

func (s *SQS) queueURLFromRequest(req protocol.Request) string {
	if raw := stringValue(req.Input, "QueueUrl"); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	hint := strings.Trim(req.QueuePathHint, "/")
	if hint == "" {
		return ""
	}
	return s.cfg.PublicBaseURL + "/" + hint
}

func (s *SQS) ensureQueueReady(queue storage.Queue, now time.Time) error {
	if queue.DeletedAt != nil || queue.AvailableAt.After(now) {
		return apierrors.ErrQueueDoesNotExist
	}
	return nil
}

func (s *SQS) sendMessageEntry(ctx context.Context, queue storage.Queue, entry map[string]any, now time.Time) (map[string]any, error) {
	body := stringValue(entry, "MessageBody")
	if err := validateMessageBody(body, queueMaximumMessageSize(queue)); err != nil {
		return nil, err
	}

	messageAttributes, attrSize, err := normalizeMessageAttributeMap(anyMapValue(entry, "MessageAttributes"), false)
	if err != nil {
		return nil, err
	}
	systemAttributes, _, err := normalizeMessageAttributeMap(anyMapValue(entry, "MessageSystemAttributes"), true)
	if err != nil {
		return nil, err
	}
	if size := len([]byte(body)) + attrSize; int64(size) > queueMaximumMessageSize(queue) {
		return nil, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "One or more parameters are invalid. Reason: Message must be shorter than the maximum allowed size.")
	}

	delaySeconds, delaySpecified := int64Field(entry, "DelaySeconds")
	if queue.FIFO && delaySpecified {
		return nil, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter DelaySeconds is invalid. Reason: The request include parameter that is not valid for this queue type.")
	}
	if !delaySpecified {
		delaySeconds = queueDelaySeconds(queue)
	}
	if delaySeconds < 0 || delaySeconds > 900 {
		return nil, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter DelaySeconds is invalid. Reason: must be between 0 and 900, if provided.")
	}

	messageGroupID := stringValue(entry, "MessageGroupId")
	messageDedupID := stringValue(entry, "MessageDeduplicationId")
	if queue.FIFO {
		if messageGroupID == "" {
			return nil, apierrors.New("MissingParameter", http.StatusBadRequest, "The request must contain the parameter MessageGroupId.")
		}
		if messageDedupID == "" {
			if queue.Attributes["ContentBasedDeduplication"] == "true" {
				sum := sha256.Sum256([]byte(body))
				messageDedupID = hex.EncodeToString(sum[:])
			} else {
				return nil, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "The queue should either have ContentBasedDeduplication enabled or MessageDeduplicationId provided explicitly.")
			}
		}
		scopeKey := dedupScopeKey(queue, messageGroupID)
		if entry, found, err := s.store.DedupEntry(ctx, queue.ID, scopeKey, messageDedupID, now); err != nil {
			return nil, apierrors.Internal("failed to lookup deduplication ledger", err)
		} else if found {
			return cloneAnyMap(entry.Response), nil
		}
	} else if messageDedupID != "" {
		return nil, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value MessageDeduplicationId for parameter MessageDeduplicationId is invalid. Reason: The request include parameter that is not valid for this queue type.")
	}

	response := map[string]any{
		"MessageId":        uuid.NewString(),
		"MD5OfMessageBody": md5Hex(body),
	}
	if len(messageAttributes) != 0 {
		md5Attrs, err := md5OfAttributeMap(messageAttributes)
		if err != nil {
			return nil, err
		}
		response["MD5OfMessageAttributes"] = md5Attrs
	}
	if len(systemAttributes) != 0 {
		md5System, err := md5OfAttributeMap(systemAttributes)
		if err != nil {
			return nil, err
		}
		response["MD5OfMessageSystemAttributes"] = md5System
	}

	sequenceNumber := ""
	if queue.FIFO {
		sequenceNumber = newSequenceNumber()
		response["SequenceNumber"] = sequenceNumber
	}

	availableAt := now.Add(time.Duration(delaySeconds) * time.Second)
	retentionDeadline := now.Add(time.Duration(queueRetentionSeconds(queue)) * time.Second)
	message := storage.Message{
		QueueID:           queue.ID,
		MessageID:         response["MessageId"].(string),
		Body:              body,
		BodyMD5:           md5Hex(body),
		MessageAttributes: messageAttributes,
		SystemAttributes:  systemAttributes,
		SentAt:            now,
		OriginalSentAt:    now,
		ReceiveCount:      0,
		AvailableAt:       availableAt,
		RetentionDeadline: retentionDeadline,
		GroupID:           messageGroupID,
		DedupID:           messageDedupID,
		SequenceNumber:    sequenceNumber,
	}
	if _, err := s.store.InsertMessage(ctx, message); err != nil {
		return nil, apierrors.Internal("failed to persist message", err)
	}

	if queue.FIFO {
		if _, err := s.store.InsertDedupEntry(ctx, storage.DedupEntry{
			QueueID:   queue.ID,
			ScopeKey:  dedupScopeKey(queue, messageGroupID),
			DedupID:   messageDedupID,
			Response:  cloneAnyMap(response),
			ExpiresAt: now.Add(fifoDedupWindow),
		}); err != nil {
			return nil, apierrors.Internal("failed to persist deduplication ledger", err)
		}
	}

	return response, nil
}

func (s *SQS) selectMessagesForReceive(ctx context.Context, queue storage.Queue, maxMessages int, now time.Time) ([]storage.Message, error) {
	for {
		messages, err := s.store.ListMessagesByQueue(ctx, queue.ID)
		if err != nil {
			return nil, apierrors.Internal("failed to list queue messages", err)
		}
		candidates := candidateMessages(queue, messages, maxMessages, now)
		if len(candidates) == 0 {
			return nil, nil
		}
		redriven := false
		for _, message := range candidates {
			if !shouldRedrive(queue, message) {
				continue
			}
			if err := s.redriveMessage(ctx, queue, message, now); err != nil {
				return nil, err
			}
			redriven = true
		}
		if redriven {
			continue
		}
		return candidates, nil
	}
}

func (s *SQS) redriveMessage(ctx context.Context, queue storage.Queue, message storage.Message, now time.Time) error {
	policy, ok, err := parseRedrivePolicy(queue.Attributes["RedrivePolicy"])
	if err != nil {
		return apierrors.ErrInvalidAttributeValue.WithCause(err)
	}
	if !ok {
		return nil
	}
	target, err := s.lookupQueueByARN(ctx, policy.TargetARN)
	if err != nil {
		return nil
	}
	if err := s.store.DeactivateReceiptsByMessage(ctx, message.RowID); err != nil {
		return apierrors.Internal("failed to deactivate receipts during redrive", err)
	}
	message.QueueID = target.ID
	message.AvailableAt = now
	message.VisibilityDeadline = nil
	message.DeadLetterSourceARN = queue.ARN
	if err := s.store.UpdateMessage(ctx, message); err != nil {
		return apierrors.Internal("failed to redrive message to DLQ", err)
	}
	return nil
}

func (s *SQS) replayReceiveAttempt(ctx context.Context, queue storage.Queue, attemptID string, visibilityTimeout int64, messageAttrNames, systemAttrNames []string, now time.Time) (map[string]any, bool, error) {
	attempt, found, err := s.store.ReceiveAttempt(ctx, queue.ID, attemptID, now)
	if err != nil {
		return nil, false, apierrors.Internal("failed to lookup receive attempt", err)
	}
	if !found {
		return nil, false, nil
	}
	for _, message := range attempt.Response {
		handle := stringValue(message, "ReceiptHandle")
		if handle == "" {
			return nil, false, nil
		}
		receipt, found, err := s.store.ReceiptByHandle(ctx, handle)
		if err != nil {
			return nil, false, apierrors.Internal("failed to lookup cached receipt handle", err)
		}
		if !found || !receipt.Active || !receipt.VisibilityDeadline.After(now) {
			return nil, false, nil
		}
		storedMessage, found, err := s.store.MessageByRowID(ctx, receipt.MessageRowID)
		if err != nil {
			return nil, false, apierrors.Internal("failed to lookup cached receive message", err)
		}
		if !found || storedMessage.QueueID != queue.ID || storedMessage.DeletedAt != nil || !storedMessage.RetentionDeadline.After(now) {
			return nil, false, nil
		}
		receipt.VisibilityDeadline = now.Add(time.Duration(visibilityTimeout) * time.Second)
		if err := s.store.UpdateReceipt(ctx, receipt); err != nil {
			return nil, false, apierrors.Internal("failed to refresh cached receipt handle", err)
		}
		storedMessage.VisibilityDeadline = &receipt.VisibilityDeadline
		if err := s.store.UpdateMessage(ctx, storedMessage); err != nil {
			return nil, false, apierrors.Internal("failed to refresh cached message visibility", err)
		}
	}

	output := map[string]any{}
	if len(attempt.Response) != 0 {
		messages := make([]any, 0, len(attempt.Response))
		for _, message := range attempt.Response {
			messages = append(messages, message)
		}
		output["Messages"] = messages
	}
	return output, true, nil
}

func (s *SQS) deleteByReceiptHandle(ctx context.Context, queue storage.Queue, handle string, now time.Time) error {
	receipt, found, err := s.store.ReceiptByHandle(ctx, handle)
	if err != nil {
		return apierrors.Internal("failed to lookup receipt handle", err)
	}
	if !found || receipt.QueueID != queue.ID {
		return apierrors.ErrReceiptHandleIsInvalid
	}
	if !receipt.Active {
		return nil
	}
	if !receipt.VisibilityDeadline.After(now) {
		return apierrors.ErrMessageNotInflight
	}

	message, found, err := s.store.MessageByRowID(ctx, receipt.MessageRowID)
	if err != nil {
		return apierrors.Internal("failed to lookup message", err)
	}
	if !found || message.DeletedAt != nil {
		receipt.Active = false
		_ = s.store.UpdateReceipt(ctx, receipt)
		return nil
	}

	message.DeletedAt = &now
	message.VisibilityDeadline = nil
	if err := s.store.UpdateMessage(ctx, message); err != nil {
		return apierrors.Internal("failed to delete message", err)
	}
	receipt.Active = false
	if err := s.store.UpdateReceipt(ctx, receipt); err != nil {
		return apierrors.Internal("failed to deactivate receipt handle", err)
	}
	if err := s.store.InvalidateReceiveAttempts(ctx, queue.ID); err != nil {
		return apierrors.Internal("failed to invalidate receive attempts", err)
	}
	return nil
}

func (s *SQS) changeVisibilityByReceiptHandle(ctx context.Context, queue storage.Queue, handle string, timeout int64, now time.Time) error {
	receipt, found, err := s.store.ReceiptByHandle(ctx, handle)
	if err != nil {
		return apierrors.Internal("failed to lookup receipt handle", err)
	}
	if !found || receipt.QueueID != queue.ID {
		return apierrors.ErrReceiptHandleIsInvalid
	}
	if !receipt.Active || !receipt.VisibilityDeadline.After(now) {
		return apierrors.ErrMessageNotInflight
	}
	maxRemaining := receipt.IssuedAt.Add(12 * time.Hour).Sub(now)
	if time.Duration(timeout)*time.Second > maxRemaining {
		return apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter VisibilityTimeout is invalid. Reason: Visibility timeout exceeds the maximum time left.")
	}

	message, found, err := s.store.MessageByRowID(ctx, receipt.MessageRowID)
	if err != nil {
		return apierrors.Internal("failed to lookup message", err)
	}
	if !found || message.DeletedAt != nil {
		return apierrors.ErrMessageNotInflight
	}

	deadline := now.Add(time.Duration(timeout) * time.Second)
	receipt.VisibilityDeadline = deadline
	if err := s.store.UpdateReceipt(ctx, receipt); err != nil {
		return apierrors.Internal("failed to update receipt visibility", err)
	}
	message.VisibilityDeadline = &deadline
	if err := s.store.UpdateMessage(ctx, message); err != nil {
		return apierrors.Internal("failed to update message visibility", err)
	}
	if err := s.store.InvalidateReceiveAttempts(ctx, queue.ID); err != nil {
		return apierrors.Internal("failed to invalidate receive attempts", err)
	}
	return nil
}

func (s *SQS) receiveMessageOutput(message storage.Message, receiptHandle string, messageAttributeNames, systemAttributeNames []string) map[string]any {
	out := map[string]any{
		"Body":          message.Body,
		"MD5OfBody":     message.BodyMD5,
		"MessageId":     message.MessageID,
		"ReceiptHandle": receiptHandle,
	}

	filteredAttrs := filterMessageAttributes(message.MessageAttributes, messageAttributeNames)
	if len(filteredAttrs) != 0 {
		out["MessageAttributes"] = filteredAttrs
		if digest, err := md5OfAttributeMap(filteredAttrs); err == nil {
			out["MD5OfMessageAttributes"] = digest
		}
	}

	systemAttrs := buildSystemAttributes(message)
	filteredSystem := filterStringMap(systemAttrs, systemAttributeNames)
	if len(filteredSystem) != 0 {
		out["Attributes"] = stringMapToAny(filteredSystem)
	}

	return out
}

func (s *SQS) materializeQueueAttributes(ctx context.Context, queue storage.Queue, now time.Time) (map[string]string, error) {
	attrs := materializeQueueAttributes(queue)
	messages, err := s.store.ListMessagesByQueue(ctx, queue.ID)
	if err != nil {
		return nil, apierrors.Internal("failed to list queue messages for attributes", err)
	}
	visible := int64(0)
	notVisible := int64(0)
	delayed := int64(0)
	for _, message := range messages {
		if message.DeletedAt != nil || !message.RetentionDeadline.After(now) {
			continue
		}
		switch {
		case message.AvailableAt.After(now):
			delayed++
		case message.VisibilityDeadline != nil && message.VisibilityDeadline.After(now):
			notVisible++
		default:
			visible++
		}
	}
	attrs["ApproximateNumberOfMessages"] = strconvFormat(visible)
	attrs["ApproximateNumberOfMessagesNotVisible"] = strconvFormat(notVisible)
	attrs["ApproximateNumberOfMessagesDelayed"] = strconvFormat(delayed)
	return attrs, nil
}

func candidateMessages(queue storage.Queue, messages []storage.Message, maxMessages int, now time.Time) []storage.Message {
	sorted := liveMessages(messages, now)
	if queue.FIFO {
		return fifoCandidateMessages(sorted, maxMessages, now)
	}
	out := make([]storage.Message, 0, maxMessages)
	for _, message := range sorted {
		if messageVisible(message, now) {
			out = append(out, message)
			if len(out) == maxMessages {
				break
			}
		}
	}
	return out
}

func fifoCandidateMessages(messages []storage.Message, maxMessages int, now time.Time) []storage.Message {
	type groupInfo struct {
		id       string
		messages []storage.Message
		head     storage.Message
	}

	lockedGroups := map[string]struct{}{}
	groups := map[string]*groupInfo{}
	order := make([]string, 0)
	for _, message := range messages {
		if message.GroupID == "" {
			continue
		}
		if message.VisibilityDeadline != nil && message.VisibilityDeadline.After(now) {
			lockedGroups[message.GroupID] = struct{}{}
		}
		info, ok := groups[message.GroupID]
		if !ok {
			groups[message.GroupID] = &groupInfo{
				id:       message.GroupID,
				messages: []storage.Message{message},
				head:     message,
			}
			order = append(order, message.GroupID)
			continue
		}
		info.messages = append(info.messages, message)
	}

	out := make([]storage.Message, 0, maxMessages)
	for _, groupID := range order {
		if len(out) == maxMessages {
			break
		}
		if _, locked := lockedGroups[groupID]; locked {
			continue
		}
		info := groups[groupID]
		if info == nil || len(info.messages) == 0 || !messageVisible(info.messages[0], now) {
			continue
		}
		for _, message := range info.messages {
			if len(out) == maxMessages {
				break
			}
			if !messageVisible(message, now) {
				break
			}
			out = append(out, message)
		}
	}
	return out
}

func liveMessages(messages []storage.Message, now time.Time) []storage.Message {
	out := make([]storage.Message, 0, len(messages))
	for _, message := range messages {
		if message.DeletedAt != nil || !message.RetentionDeadline.After(now) {
			continue
		}
		out = append(out, message)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SentAt.Equal(out[j].SentAt) {
			return out[i].RowID < out[j].RowID
		}
		return out[i].SentAt.Before(out[j].SentAt)
	})
	return out
}

func messageVisible(message storage.Message, now time.Time) bool {
	if message.DeletedAt != nil || !message.RetentionDeadline.After(now) {
		return false
	}
	if message.AvailableAt.After(now) {
		return false
	}
	if message.VisibilityDeadline != nil && message.VisibilityDeadline.After(now) {
		return false
	}
	return true
}

func shouldRedrive(queue storage.Queue, message storage.Message) bool {
	policy, ok, err := parseRedrivePolicy(queue.Attributes["RedrivePolicy"])
	if err != nil || !ok {
		return false
	}
	return message.ReceiveCount >= policy.MaxReceiveCount
}

func parseRedrivePolicy(raw string) (redrivePolicy, bool, error) {
	if strings.TrimSpace(raw) == "" {
		return redrivePolicy{}, false, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return redrivePolicy{}, false, err
	}
	policy := redrivePolicy{TargetARN: stringValue(decoded, "deadLetterTargetArn")}
	maxReceiveCount, err := int64Value(decoded, "maxReceiveCount", 0)
	if err != nil {
		return redrivePolicy{}, false, err
	}
	policy.MaxReceiveCount = maxReceiveCount
	if policy.TargetARN == "" || policy.MaxReceiveCount <= 0 {
		return redrivePolicy{}, false, nil
	}
	return policy, true, nil
}

func dedupScopeKey(queue storage.Queue, messageGroupID string) string {
	if queue.Attributes["DeduplicationScope"] == "messageGroup" {
		return messageGroupID
	}
	return queue.Name
}

func queueVisibilityTimeout(queue storage.Queue) int64 {
	value, err := strconv.ParseInt(queue.Attributes["VisibilityTimeout"], 10, 64)
	if err != nil || value < 0 {
		return 30
	}
	return value
}

func queueMaximumMessageSize(queue storage.Queue) int64 {
	value, err := strconv.ParseInt(queue.Attributes["MaximumMessageSize"], 10, 64)
	if err != nil || value <= 0 {
		return 1024 * 1024
	}
	return value
}

func queueRetentionSeconds(queue storage.Queue) int64 {
	value, err := strconv.ParseInt(queue.Attributes["MessageRetentionPeriod"], 10, 64)
	if err != nil || value <= 0 {
		return 345600
	}
	return value
}

func queueDelaySeconds(queue storage.Queue) int64 {
	value, err := strconv.ParseInt(queue.Attributes["DelaySeconds"], 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func queueWaitTime(queue storage.Queue) int64 {
	value, err := strconv.ParseInt(queue.Attributes["ReceiveMessageWaitTimeSeconds"], 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func buildSystemAttributes(message storage.Message) map[string]string {
	out := map[string]string{
		"SentTimestamp":           strconvFormat(message.SentAt.UnixMilli()),
		"ApproximateReceiveCount": strconvFormat(message.ReceiveCount),
		"SenderId":                "AIDALOCALTEST",
	}
	if message.FirstReceivedAt != nil {
		out["ApproximateFirstReceiveTimestamp"] = strconvFormat(message.FirstReceivedAt.UnixMilli())
	}
	if message.SequenceNumber != "" {
		out["SequenceNumber"] = message.SequenceNumber
	}
	if message.GroupID != "" {
		out["MessageGroupId"] = message.GroupID
	}
	if message.DedupID != "" {
		out["MessageDeduplicationId"] = message.DedupID
	}
	if message.DeadLetterSourceARN != "" {
		out["DeadLetterQueueSourceArn"] = message.DeadLetterSourceARN
	}
	if raw, ok := message.SystemAttributes["AWSTraceHeader"]; ok {
		if attr, ok := raw.(map[string]any); ok {
			if value := stringValue(attr, "StringValue"); value != "" {
				out["AWSTraceHeader"] = value
			}
		}
	}
	return out
}

func filterMessageAttributes(attrs map[string]any, names []string) map[string]any {
	if len(attrs) == 0 {
		return nil
	}
	if len(names) == 0 {
		return nil
	}
	out := map[string]any{}
	all := containsWildcard(names, "All") || containsWildcard(names, ".*")
	for key, value := range attrs {
		if all || matchesAttributeSelector(key, names) {
			out[key] = value
		}
	}
	return out
}

func filterStringMap(values map[string]string, names []string) map[string]string {
	if len(values) == 0 || len(names) == 0 {
		return nil
	}
	out := map[string]string{}
	if containsWildcard(names, "All") {
		maps.Copy(out, values)
		return out
	}
	for _, name := range names {
		if value, ok := values[name]; ok {
			out[name] = value
		}
	}
	return out
}

func containsWildcard(values []string, needle string) bool {
	return slices.Contains(values, needle)
}

func matchesAttributeSelector(name string, selectors []string) bool {
	for _, selector := range selectors {
		if selector == name {
			return true
		}
		if strings.HasSuffix(selector, ".*") && strings.HasPrefix(name, strings.TrimSuffix(selector, "*")) {
			return true
		}
	}
	return false
}

func normalizeMessageAttributeMap(raw map[string]any, system bool) (map[string]any, int, error) {
	if len(raw) == 0 {
		return nil, 0, nil
	}
	if len(raw) > 10 && !system {
		return nil, 0, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: a maximum of 10 message attributes is allowed.")
	}

	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]any, len(raw))
	size := 0
	for _, name := range names {
		if err := validateMessageAttributeName(name, system); err != nil {
			return nil, 0, err
		}
		value, ok := raw[name].(map[string]any)
		if !ok {
			return nil, 0, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: message attribute value must be a structure.")
		}
		dataType := stringValue(value, "DataType")
		if dataType == "" {
			return nil, 0, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: DataType is required.")
		}
		if list := anySliceValue(value, "StringListValues"); len(list) != 0 {
			return nil, 0, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: StringListValues is not implemented.")
		}
		if list := anySliceValue(value, "BinaryListValues"); len(list) != 0 {
			return nil, 0, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: BinaryListValues is not implemented.")
		}

		normalized := map[string]any{
			"DataType": dataType,
		}
		size += len([]byte(name)) + len([]byte(dataType))
		switch {
		case strings.HasPrefix(dataType, "Binary"):
			rawBinary := stringValue(value, "BinaryValue")
			if rawBinary == "" {
				return nil, 0, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: BinaryValue is required.")
			}
			if _, err := decodeBinaryValue(rawBinary); err != nil {
				return nil, 0, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: BinaryValue must be valid base64.").WithCause(err)
			}
			normalized["BinaryValue"] = rawBinary
			size += len(rawBinary)
		default:
			rawString := stringValue(value, "StringValue")
			if rawString == "" {
				return nil, 0, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: StringValue is required.")
			}
			if err := validateMessageCharacters(rawString); err != nil {
				return nil, 0, err
			}
			normalized["StringValue"] = rawString
			size += len([]byte(rawString))
		}
		if system {
			if name != "AWSTraceHeader" {
				return nil, 0, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageSystemAttributes is invalid. Reason: only AWSTraceHeader is supported.")
			}
			if !strings.HasPrefix(dataType, "String") {
				return nil, 0, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageSystemAttributes is invalid. Reason: AWSTraceHeader must use the String data type.")
			}
		}
		out[name] = normalized
	}
	return out, size, nil
}

func validateMessageAttributeName(name string, system bool) error {
	if !attrNamePattern.MatchString(name) {
		return apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: attribute names may only contain alphanumeric characters, hyphens, underscores, and periods.")
	}
	if strings.HasPrefix(strings.ToLower(name), "aws.") || strings.HasPrefix(strings.ToLower(name), "amazon.") {
		return apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: attribute names may not start with AWS. or Amazon.")
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageAttributes is invalid. Reason: attribute names must not start or end with a period and may not contain consecutive periods.")
	}
	if system && name != "AWSTraceHeader" {
		return apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageSystemAttributes is invalid. Reason: only AWSTraceHeader is supported.")
	}
	return nil
}

func validateMessageBody(body string, maxBytes int64) error {
	if body == "" {
		return apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MessageBody is invalid. Reason: message body must not be empty.")
	}
	if int64(len([]byte(body))) > maxBytes {
		return apierrors.New("InvalidParameterValue", http.StatusBadRequest, "One or more parameters are invalid. Reason: Message must be shorter than the maximum allowed size.")
	}
	return validateMessageCharacters(body)
}

func validateMessageCharacters(body string) error {
	for _, r := range body {
		switch {
		case r == 0x9 || r == 0xA || r == 0xD:
		case r >= 0x20 && r <= 0xD7FF:
		case r >= 0xE000 && r <= 0xFFFD:
		case r >= 0x10000 && r <= 0x10FFFF:
		default:
			return apierrors.ErrInvalidMessageContents
		}
	}
	return nil
}

func md5Hex(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

func md5OfAttributeMap(attrs map[string]any) (string, error) {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	for _, name := range names {
		value, ok := attrs[name].(map[string]any)
		if !ok {
			return "", fmt.Errorf("attribute %q is not a structure", name)
		}
		dataType := stringValue(value, "DataType")
		writeWithLength(&buf, []byte(name))
		writeWithLength(&buf, []byte(dataType))
		if strings.HasPrefix(dataType, "Binary") {
			buf.WriteByte(2)
			rawBinary := stringValue(value, "BinaryValue")
			decoded, err := decodeBinaryValue(rawBinary)
			if err != nil {
				return "", err
			}
			writeWithLength(&buf, decoded)
			continue
		}
		buf.WriteByte(1)
		writeWithLength(&buf, []byte(stringValue(value, "StringValue")))
	}

	sum := md5.Sum(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func decodeBinaryValue(raw string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		return decoded, nil
	}
	return nil, fmt.Errorf("invalid base64 value")
}

func writeWithLength(buf *bytes.Buffer, value []byte) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	buf.Write(size[:])
	buf.Write(value)
}

func estimateMessageSize(body string, attrs map[string]any) int {
	size := len([]byte(body))
	for name, raw := range attrs {
		size += len([]byte(name))
		value, _ := raw.(map[string]any)
		size += len([]byte(stringValue(value, "DataType")))
		size += len([]byte(stringValue(value, "StringValue")))
		size += len([]byte(stringValue(value, "BinaryValue")))
	}
	return size
}

func newReceiptHandle(queueID, messageRowID int64) string {
	payload := make([]byte, 24)
	binary.BigEndian.PutUint64(payload[0:8], uint64(queueID))
	binary.BigEndian.PutUint64(payload[8:16], uint64(messageRowID))
	if _, err := rand.Read(payload[16:]); err != nil {
		copy(payload[16:], []byte(uuid.NewString())[:8])
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func newSequenceNumber() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return strconvFormat(time.Now().UTC().UnixNano())
	}
	return new(big.Int).SetBytes(raw).String()
}

func batchFailure(id string, err error) map[string]any {
	apiErr, ok := apierrors.As(err)
	if !ok {
		apiErr = apierrors.Internal("internal server error", err)
	}
	return map[string]any{
		"Id":          id,
		"Code":        apiErr.Code,
		"Message":     apiErr.Message,
		"SenderFault": apiErr.Fault == apierrors.SenderFault,
	}
}

func validateBatchRequestEntries(entries []any) ([]map[string]any, error) {
	if len(entries) == 0 {
		return nil, apierrors.ErrEmptyBatchRequest
	}
	if len(entries) > 10 {
		return nil, apierrors.ErrTooManyEntriesInBatch
	}
	out := make([]map[string]any, 0, len(entries))
	seen := map[string]struct{}{}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, apierrors.ErrInvalidBatchEntryID
		}
		id := stringValue(entry, "Id")
		if !batchEntryIDPattern.MatchString(id) {
			return nil, apierrors.ErrInvalidBatchEntryID
		}
		if _, exists := seen[id]; exists {
			return nil, apierrors.ErrBatchEntryIDsNotDistinct
		}
		seen[id] = struct{}{}
		out = append(out, entry)
	}
	return out, nil
}

func unionStringSets(left, right []string) []string {
	if len(left) == 0 {
		return append([]string(nil), right...)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(left)+len(right))
	for _, value := range left {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for _, value := range right {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func int64Field(input map[string]any, key string) (int64, bool) {
	raw, ok := input[key]
	if !ok || raw == nil {
		return 0, false
	}
	value, err := int64Value(input, key, 0)
	if err != nil {
		return 0, true
	}
	return value, true
}

func anyMapValue(input map[string]any, key string) map[string]any {
	raw, ok := input[key]
	if !ok || raw == nil {
		return nil
	}
	if typed, ok := raw.(map[string]any); ok {
		return typed
	}
	return nil
}

func anySliceValue(input map[string]any, key string) []any {
	raw, ok := input[key]
	if !ok || raw == nil {
		return nil
	}
	switch typed := raw.(type) {
	case []any:
		return append([]any(nil), typed...)
	case []map[string]any:
		out := make([]any, 0, len(typed))
		for _, entry := range typed {
			out = append(out, entry)
		}
		return out
	default:
		return nil
	}
}

func sliceOfMaps(values []any) []map[string]any {
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if typed, ok := value.(map[string]any); ok {
			out = append(out, typed)
		}
	}
	return out
}

func cloneAnyMap(values map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	maps.Copy(out, values)
	return out
}
