package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"emulator-aws-sqs/internal/clock"
	"emulator-aws-sqs/internal/config"
	apierrors "emulator-aws-sqs/internal/errors"
	"emulator-aws-sqs/internal/protocol"
	"emulator-aws-sqs/internal/storage"
)

var queueNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}(\.fifo)?$`)

type SQS struct {
	store storage.Store
	clock clock.Clock
	cfg   config.Config
}

func NewSQS(store storage.Store, clk clock.Clock, cfg config.Config) *Dispatcher {
	svc := &SQS{store: store, clock: clk, cfg: cfg}
	dispatcher := NewDispatcher()
	dispatcher.Register("CreateQueue", svc.CreateQueue)
	dispatcher.Register("DeleteQueue", svc.DeleteQueue)
	dispatcher.Register("GetQueueUrl", svc.GetQueueURL)
	dispatcher.Register("ListQueues", svc.ListQueues)
	dispatcher.Register("GetQueueAttributes", svc.GetQueueAttributes)
	dispatcher.Register("SetQueueAttributes", svc.SetQueueAttributes)
	dispatcher.Register("TagQueue", svc.TagQueue)
	dispatcher.Register("UntagQueue", svc.UntagQueue)
	dispatcher.Register("ListQueueTags", svc.ListQueueTags)
	dispatcher.Register("ListDeadLetterSourceQueues", svc.ListDeadLetterSourceQueues)
	dispatcher.Register("AddPermission", svc.AddPermission)
	dispatcher.Register("RemovePermission", svc.RemovePermission)
	dispatcher.Register("PurgeQueue", svc.PurgeQueue)
	dispatcher.Register("ListMessageMoveTasks", svc.ListMessageMoveTasks)
	dispatcher.Register("StartMessageMoveTask", svc.StartMessageMoveTask)
	dispatcher.Register("CancelMessageMoveTask", svc.CancelMessageMoveTask)
	return dispatcher
}

func (s *SQS) CreateQueue(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	now := s.clock.Now()
	if err := s.store.ApplyDueQueueMutations(ctx, now); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to apply queue mutations", err)
	}

	name := stringValue(req.Input, "QueueName")
	if !validQueueName(name) {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter QueueName is invalid. Must be 1 to 80 alphanumeric characters, hyphen, or underscore.").WithCause(fmt.Errorf("queue name %q", name))
	}
	attrs := defaultQueueAttributes()
	for key, value := range stringMapValue(req.Input, "Attributes") {
		attrs[key] = normalizePolicyJSON(value)
	}
	if err := validateQueueAttributes(name, attrs); err != nil {
		return protocol.Response{}, err
	}

	existing, found, err := s.store.QueueByName(ctx, req.Identity.AccountID, s.cfg.Region, name)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to lookup queue", err)
	}
	if found {
		if existing.DeletedAt != nil && existing.ReuseBlockedTill != nil && now.Before(*existing.ReuseBlockedTill) {
			return protocol.Response{}, apierrors.ErrQueueDeletedRecently
		}
		if existing.DeletedAt != nil {
			if err := s.store.DeleteQueueRow(ctx, existing.ID); err != nil {
				return protocol.Response{}, apierrors.Internal("failed to clear queue tombstone", err)
			}
		} else if !queueAttributesEquivalent(existing, attrs) {
			return protocol.Response{}, apierrors.ErrQueueNameExists
		} else {
			return protocol.Response{Output: map[string]any{"QueueUrl": existing.URL}}, nil
		}
	}

	fifo := attrs["FifoQueue"] == "true"
	queue := storage.Queue{
		AccountID:      req.Identity.AccountID,
		Region:         s.cfg.Region,
		Name:           name,
		URL:            s.queueURL(req.Identity.AccountID, name),
		ARN:            s.queueARN(req.Identity.AccountID, name),
		FIFO:           fifo,
		Attributes:     attrs,
		CreatedAt:      now,
		AvailableAt:    now.Add(s.cfg.CreatePropagation),
		LastModifiedAt: now,
	}
	queue.Attributes["QueueArn"] = queue.ARN
	queue.Attributes["CreatedTimestamp"] = strconvFormat(now.Unix())
	queue.Attributes["LastModifiedTimestamp"] = strconvFormat(now.Unix())
	created, err := s.store.InsertQueue(ctx, queue)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to create queue", err)
	}
	return protocol.Response{Output: map[string]any{"QueueUrl": created.URL}}, nil
}

func (s *SQS) DeleteQueue(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queue, err := s.lookupQueueByURL(ctx, stringValue(req.Input, "QueueUrl"))
	if err != nil {
		return protocol.Response{}, err
	}
	now := s.clock.Now()
	queue.DeletedAt = &now
	reuse := now.Add(s.cfg.DeleteCooldown)
	queue.ReuseBlockedTill = &reuse
	queue.LastModifiedAt = now
	queue.Attributes["LastModifiedTimestamp"] = strconvFormat(now.Unix())
	if err := s.store.UpdateQueue(ctx, queue); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to delete queue", err)
	}
	return protocol.Response{}, nil
}

func (s *SQS) GetQueueURL(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queueName := stringValue(req.Input, "QueueName")
	queue, found, err := s.store.QueueByName(ctx, req.Identity.AccountID, s.cfg.Region, queueName)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to lookup queue", err)
	}
	if !found || queue.DeletedAt != nil {
		return protocol.Response{}, apierrors.ErrQueueDoesNotExist
	}
	return protocol.Response{Output: map[string]any{"QueueUrl": queue.URL}}, nil
}

func (s *SQS) ListQueues(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	if err := s.store.ApplyDueQueueMutations(ctx, s.clock.Now()); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to apply queue mutations", err)
	}
	prefix := stringValue(req.Input, "QueueNamePrefix")
	maxResults, err := int64Value(req.Input, "MaxResults", 0)
	if err != nil {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "MaxResults must be a valid integer.").WithCause(err)
	}
	offset, err := decodeNextToken(stringValue(req.Input, "NextToken"))
	if err != nil {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "The specified next token is invalid.").WithCause(err)
	}
	queues, err := s.store.ListQueues(ctx, req.Identity.AccountID, s.cfg.Region, prefix)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to list queues", err)
	}
	filtered := make([]string, 0, len(queues))
	for _, queue := range queues {
		if queue.DeletedAt != nil {
			continue
		}
		filtered = append(filtered, queue.URL)
	}
	if offset > len(filtered) {
		offset = len(filtered)
	}
	page := filtered[offset:]
	nextToken := ""
	if maxResults > 0 && int(maxResults) < len(page) {
		nextToken = encodeNextToken(offset + int(maxResults))
		page = page[:maxResults]
	}
	output := map[string]any{"QueueUrls": toAnySlice(page)}
	if nextToken != "" {
		output["NextToken"] = nextToken
	}
	return protocol.Response{Output: output}, nil
}

func (s *SQS) GetQueueAttributes(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queue, err := s.lookupQueueByURL(ctx, stringValue(req.Input, "QueueUrl"))
	if err != nil {
		return protocol.Response{}, err
	}
	if err := s.store.ApplyDueQueueMutations(ctx, s.clock.Now()); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to apply queue mutations", err)
	}
	queue, err = s.lookupQueueByURL(ctx, queue.URL)
	if err != nil {
		return protocol.Response{}, err
	}
	requested := stringSliceValue(req.Input, "AttributeNames")
	if len(requested) == 0 {
		requested = []string{"All"}
	}
	attrs := materializeQueueAttributes(queue)
	result := map[string]string{}
	if containsFold(requested, "All") {
		for key, value := range attrs {
			result[key] = value
		}
	} else {
		for _, key := range requested {
			if value, ok := attrs[key]; ok {
				result[key] = value
			}
		}
	}
	return protocol.Response{Output: map[string]any{"Attributes": stringMapToAny(result)}}, nil
}

func (s *SQS) SetQueueAttributes(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queue, err := s.lookupQueueByURL(ctx, stringValue(req.Input, "QueueUrl"))
	if err != nil {
		return protocol.Response{}, err
	}
	patch := stringMapValue(req.Input, "Attributes")
	if len(patch) == 0 {
		return protocol.Response{}, nil
	}
	next := cloneMap(queue.Attributes)
	for key, value := range patch {
		next[key] = normalizePolicyJSON(value)
	}
	if err := validateQueueAttributes(queue.Name, next); err != nil {
		return protocol.Response{}, err
	}
	now := s.clock.Now()
	queue.LastModifiedAt = now
	queue.Attributes["LastModifiedTimestamp"] = strconvFormat(now.Unix())
	if err := s.store.UpdateQueue(ctx, queue); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to update queue metadata", err)
	}
	effectiveAt := now.Add(s.cfg.AttributePropagation)
	if onlyRetention(patch) {
		effectiveAt = now.Add(s.cfg.RetentionPropagation)
	}
	if err := s.store.SavePendingAttributes(ctx, queue.ID, patch, effectiveAt, now); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to save pending attributes", err)
	}
	return protocol.Response{}, nil
}

func (s *SQS) TagQueue(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queue, err := s.lookupQueueByURL(ctx, stringValue(req.Input, "QueueUrl"))
	if err != nil {
		return protocol.Response{}, err
	}
	current, err := s.store.ListTags(ctx, queue.ID)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to list tags", err)
	}
	for key, value := range stringMapValue(req.Input, "Tags") {
		current[key] = value
	}
	if err := s.store.ReplaceTags(ctx, queue.ID, current); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to store tags", err)
	}
	return protocol.Response{}, nil
}

func (s *SQS) UntagQueue(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queue, err := s.lookupQueueByURL(ctx, stringValue(req.Input, "QueueUrl"))
	if err != nil {
		return protocol.Response{}, err
	}
	if err := s.store.DeleteTags(ctx, queue.ID, stringSliceValue(req.Input, "TagKeys")); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to remove tags", err)
	}
	return protocol.Response{}, nil
}

func (s *SQS) ListQueueTags(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queue, err := s.lookupQueueByURL(ctx, stringValue(req.Input, "QueueUrl"))
	if err != nil {
		return protocol.Response{}, err
	}
	tags, err := s.store.ListTags(ctx, queue.ID)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to list tags", err)
	}
	return protocol.Response{Output: map[string]any{"Tags": stringMapToAny(tags)}}, nil
}

func (s *SQS) ListDeadLetterSourceQueues(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queue, err := s.lookupQueueByURL(ctx, stringValue(req.Input, "QueueUrl"))
	if err != nil {
		return protocol.Response{}, err
	}
	maxResults, err := int64Value(req.Input, "MaxResults", 0)
	if err != nil {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "MaxResults must be a valid integer.").WithCause(err)
	}
	offset, err := decodeNextToken(stringValue(req.Input, "NextToken"))
	if err != nil {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "The specified next token is invalid.").WithCause(err)
	}
	queues, err := s.store.ListQueuesByRedriveTarget(ctx, queue.ARN)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to list DLQ sources", err)
	}
	urls := make([]string, 0, len(queues))
	for _, source := range queues {
		if source.DeletedAt == nil {
			urls = append(urls, source.URL)
		}
	}
	sort.Strings(urls)
	if offset > len(urls) {
		offset = len(urls)
	}
	page := urls[offset:]
	nextToken := ""
	if maxResults > 0 && int(maxResults) < len(page) {
		nextToken = encodeNextToken(offset + int(maxResults))
		page = page[:maxResults]
	}
	output := map[string]any{"queueUrls": toAnySlice(page)}
	if nextToken != "" {
		output["NextToken"] = nextToken
	}
	return protocol.Response{Output: output}, nil
}

func (s *SQS) AddPermission(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queue, err := s.lookupQueueByURL(ctx, stringValue(req.Input, "QueueUrl"))
	if err != nil {
		return protocol.Response{}, err
	}
	label := stringValue(req.Input, "Label")
	if label == "" || len(label) > 80 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(label) {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter Label is invalid.")
	}
	accounts := stringSliceValue(req.Input, "AWSAccountIds")
	actions := stringSliceValue(req.Input, "Actions")
	if len(actions) > 7 {
		return protocol.Response{}, apierrors.New("OverLimit", http.StatusBadRequest, "An Amazon SQS policy can have a maximum of seven actions per statement.")
	}
	policy, err := parseJSONDocument(queue.Attributes["Policy"])
	if err != nil {
		return protocol.Response{}, apierrors.ErrInvalidAttributeValue.WithCause(err)
	}
	if len(policy) == 0 {
		policy = map[string]any{
			"Version":   "2012-10-17",
			"Id":        queue.ARN + "/SQSDefaultPolicy",
			"Statement": []any{},
		}
	}
	statements, _ := policy["Statement"].([]any)
	filtered := make([]any, 0, len(statements))
	for _, statement := range statements {
		stmt, _ := statement.(map[string]any)
		if stringValue(stmt, "Sid") == label {
			continue
		}
		filtered = append(filtered, statement)
	}
	actionsOut := make([]any, 0, len(actions))
	for _, action := range actions {
		actionsOut = append(actionsOut, "sqs:"+action)
	}
	accountsOut := make([]any, 0, len(accounts))
	for _, account := range accounts {
		accountsOut = append(accountsOut, account)
	}
	filtered = append(filtered, map[string]any{
		"Sid":       label,
		"Effect":    "Allow",
		"Principal": map[string]any{"AWS": accountsOut},
		"Action":    actionsOut,
		"Resource":  queue.ARN,
	})
	policy["Statement"] = filtered
	raw, err := json.Marshal(policy)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to marshal policy", err)
	}
	queue.Attributes["Policy"] = string(raw)
	queue.LastModifiedAt = s.clock.Now()
	queue.Attributes["LastModifiedTimestamp"] = strconvFormat(queue.LastModifiedAt.Unix())
	if err := s.store.UpdateQueue(ctx, queue); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to update queue", err)
	}
	return protocol.Response{}, nil
}

func (s *SQS) RemovePermission(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queue, err := s.lookupQueueByURL(ctx, stringValue(req.Input, "QueueUrl"))
	if err != nil {
		return protocol.Response{}, err
	}
	label := stringValue(req.Input, "Label")
	if label == "" {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter Label is invalid.")
	}
	policy, err := parseJSONDocument(queue.Attributes["Policy"])
	if err != nil {
		return protocol.Response{}, apierrors.ErrInvalidAttributeValue.WithCause(err)
	}
	statements, _ := policy["Statement"].([]any)
	filtered := make([]any, 0, len(statements))
	for _, statement := range statements {
		stmt, _ := statement.(map[string]any)
		if stringValue(stmt, "Sid") == label {
			continue
		}
		filtered = append(filtered, statement)
	}
	policy["Statement"] = filtered
	raw, err := json.Marshal(policy)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to marshal policy", err)
	}
	queue.Attributes["Policy"] = string(raw)
	queue.LastModifiedAt = s.clock.Now()
	queue.Attributes["LastModifiedTimestamp"] = strconvFormat(queue.LastModifiedAt.Unix())
	if err := s.store.UpdateQueue(ctx, queue); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to update queue", err)
	}
	return protocol.Response{}, nil
}

func (s *SQS) PurgeQueue(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	queue, err := s.lookupQueueByURL(ctx, stringValue(req.Input, "QueueUrl"))
	if err != nil {
		return protocol.Response{}, err
	}
	now := s.clock.Now()
	if queue.PurgeExpiresAt != nil && now.Before(*queue.PurgeExpiresAt) {
		return protocol.Response{}, apierrors.ErrPurgeQueueInProgress
	}
	queue.PurgeRequestedAt = &now
	expires := now.Add(s.cfg.PurgeCooldown)
	queue.PurgeExpiresAt = &expires
	queue.LastModifiedAt = now
	queue.Attributes["LastModifiedTimestamp"] = strconvFormat(now.Unix())
	if err := s.store.UpdateQueue(ctx, queue); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to purge queue", err)
	}
	return protocol.Response{}, nil
}

func (s *SQS) ListMessageMoveTasks(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	source, err := s.lookupQueueByARN(ctx, stringValue(req.Input, "SourceArn"))
	if err != nil {
		return protocol.Response{}, apierrors.ErrResourceNotFoundException
	}
	maxResults, err := int64Value(req.Input, "MaxResults", 1)
	if err != nil {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "MaxResults must be a valid integer.").WithCause(err)
	}
	if maxResults <= 0 || maxResults > 10 {
		return protocol.Response{}, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "Value for parameter MaxResults is invalid. Reason: must be between 1 and 10, if provided.")
	}
	tasks, err := s.store.ListMoveTasksBySourceQueue(ctx, source.ID, int(maxResults))
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to list message move tasks", err)
	}
	results := make([]any, 0, len(tasks))
	for _, task := range tasks {
		entry := map[string]any{
			"Status":                           task.Status,
			"SourceArn":                        source.ARN,
			"ApproximateNumberOfMessagesMoved": task.ApproximateNumberOfMessagesMoved,
			"StartedTimestamp":                 task.StartedAt.UnixMilli(),
		}
		if task.Status == "RUNNING" || task.Status == "CANCELLING" {
			entry["TaskHandle"] = task.TaskHandle
		}
		if task.MaxMessagesPerSecond != nil {
			entry["MaxNumberOfMessagesPerSecond"] = *task.MaxMessagesPerSecond
		}
		if task.ApproximateNumberOfMessagesToMove != nil {
			entry["ApproximateNumberOfMessagesToMove"] = *task.ApproximateNumberOfMessagesToMove
		}
		if task.DestinationQueueID != nil {
			if destination, found, err := s.store.QueueByID(ctx, *task.DestinationQueueID); err == nil && found {
				entry["DestinationArn"] = destination.ARN
			}
		}
		if task.FailureReason != "" {
			entry["FailureReason"] = task.FailureReason
		}
		results = append(results, entry)
	}
	return protocol.Response{Output: map[string]any{"Results": results}}, nil
}

func (s *SQS) StartMessageMoveTask(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	source, err := s.lookupQueueByARN(ctx, stringValue(req.Input, "SourceArn"))
	if err != nil {
		return protocol.Response{}, apierrors.ErrResourceNotFoundException
	}
	if running, found, err := s.store.FindRunningMoveTask(ctx, source.ID); err != nil {
		return protocol.Response{}, apierrors.Internal("failed to inspect move tasks", err)
	} else if found {
		return protocol.Response{Output: map[string]any{"TaskHandle": running.TaskHandle}}, nil
	}
	now := s.clock.Now()
	task := storage.MoveTask{
		TaskHandle:                       "AQEB" + encodeNextToken(int(now.UnixNano())),
		SourceQueueID:                    source.ID,
		Status:                           "RUNNING",
		ApproximateNumberOfMessagesMoved: 0,
		StartedAt:                        now,
		UpdatedAt:                        now,
	}
	if destArn := stringValue(req.Input, "DestinationArn"); destArn != "" {
		destination, err := s.lookupQueueByARN(ctx, destArn)
		if err != nil {
			return protocol.Response{}, apierrors.ErrResourceNotFoundException
		}
		task.DestinationQueueID = &destination.ID
	}
	if maxPerSecond, err := int64Value(req.Input, "MaxNumberOfMessagesPerSecond", 0); err == nil && maxPerSecond > 0 {
		task.MaxMessagesPerSecond = &maxPerSecond
	}
	created, err := s.store.InsertMoveTask(ctx, task)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to create move task", err)
	}
	return protocol.Response{Output: map[string]any{"TaskHandle": created.TaskHandle}}, nil
}

func (s *SQS) CancelMessageMoveTask(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	handle := stringValue(req.Input, "TaskHandle")
	source, err := s.lookupQueueByARN(ctx, stringValue(req.Input, "SourceArn"))
	if err != nil {
		return protocol.Response{}, apierrors.ErrResourceNotFoundException
	}
	tasks, err := s.store.ListMoveTasksBySourceQueue(ctx, source.ID, 10)
	if err != nil {
		return protocol.Response{}, apierrors.Internal("failed to list move tasks", err)
	}
	for _, task := range tasks {
		if task.TaskHandle != handle {
			continue
		}
		now := s.clock.Now()
		task.Status = "CANCELLED"
		task.CompletedAt = &now
		task.UpdatedAt = now
		if err := s.store.UpdateMoveTask(ctx, task); err != nil {
			return protocol.Response{}, apierrors.Internal("failed to cancel move task", err)
		}
		return protocol.Response{Output: map[string]any{"ApproximateNumberOfMessagesMoved": task.ApproximateNumberOfMessagesMoved}}, nil
	}
	return protocol.Response{}, apierrors.ErrResourceNotFoundException
}

func (s *SQS) lookupQueueByURL(ctx context.Context, queueURL string) (storage.Queue, error) {
	queue, found, err := s.store.QueueByURL(ctx, queueURL)
	if err != nil {
		return storage.Queue{}, apierrors.Internal("failed to lookup queue", err)
	}
	if !found || queue.DeletedAt != nil {
		return storage.Queue{}, apierrors.ErrQueueDoesNotExist
	}
	return queue, nil
}

func (s *SQS) lookupQueueByARN(ctx context.Context, queueARN string) (storage.Queue, error) {
	queue, found, err := s.store.QueueByARN(ctx, queueARN)
	if err != nil {
		return storage.Queue{}, apierrors.Internal("failed to lookup queue", err)
	}
	if !found || queue.DeletedAt != nil {
		return storage.Queue{}, apierrors.ErrResourceNotFoundException
	}
	return queue, nil
}

func (s *SQS) queueURL(accountID string, name string) string {
	return fmt.Sprintf("%s/%s/%s", s.cfg.PublicBaseURL, accountID, name)
}

func (s *SQS) queueARN(accountID string, name string) string {
	return fmt.Sprintf("arn:aws:sqs:%s:%s:%s", s.cfg.Region, accountID, name)
}

func validQueueName(name string) bool {
	return queueNamePattern.MatchString(name)
}

func defaultQueueAttributes() map[string]string {
	return map[string]string{
		"VisibilityTimeout":             "30",
		"MaximumMessageSize":            "1048576",
		"MessageRetentionPeriod":        "345600",
		"DelaySeconds":                  "0",
		"ReceiveMessageWaitTimeSeconds": "0",
		"Policy":                        "",
		"RedrivePolicy":                 "",
		"FifoQueue":                     "false",
		"ContentBasedDeduplication":     "false",
		"KmsMasterKeyId":                "",
		"KmsDataKeyReusePeriodSeconds":  "300",
		"DeduplicationScope":            "queue",
		"FifoThroughputLimit":           "perQueue",
		"RedriveAllowPolicy":            "",
		"SqsManagedSseEnabled":          "false",
	}
}

func validateQueueAttributes(name string, attrs map[string]string) error {
	fifo := attrs["FifoQueue"] == "true"
	if fifo && !strings.HasSuffix(name, ".fifo") {
		return apierrors.New("InvalidParameterValue", http.StatusBadRequest, "The name of a FIFO queue can only include alphanumeric characters, hyphens, underscores, and must end with .fifo.")
	}
	if !fifo && strings.HasSuffix(name, ".fifo") {
		return apierrors.New("InvalidParameterValue", http.StatusBadRequest, "The name of a FIFO queue must end with the .fifo suffix.")
	}
	if value := attrs["VisibilityTimeout"]; value != "" && !intBetween(value, 0, 43200) {
		return apierrors.ErrInvalidAttributeValue
	}
	if value := attrs["MaximumMessageSize"]; value != "" && !intBetween(value, 1024, 1048576) {
		return apierrors.ErrInvalidAttributeValue
	}
	if value := attrs["MessageRetentionPeriod"]; value != "" && !intBetween(value, 60, 1209600) {
		return apierrors.ErrInvalidAttributeValue
	}
	if value := attrs["DelaySeconds"]; value != "" && !intBetween(value, 0, 900) {
		return apierrors.ErrInvalidAttributeValue
	}
	if value := attrs["ReceiveMessageWaitTimeSeconds"]; value != "" && !intBetween(value, 0, 20) {
		return apierrors.ErrInvalidAttributeValue
	}
	if value := attrs["KmsDataKeyReusePeriodSeconds"]; value != "" && !intBetween(value, 60, 86400) {
		return apierrors.ErrInvalidAttributeValue
	}
	return nil
}

func queueAttributesEquivalent(existing storage.Queue, attrs map[string]string) bool {
	current := cloneMap(existing.Attributes)
	delete(current, "CreatedTimestamp")
	delete(current, "LastModifiedTimestamp")
	delete(current, "QueueArn")
	expected := cloneMap(attrs)
	delete(expected, "CreatedTimestamp")
	delete(expected, "LastModifiedTimestamp")
	delete(expected, "QueueArn")
	if len(current) != len(expected) {
		return false
	}
	for key, value := range expected {
		if current[key] != value {
			return false
		}
	}
	return true
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func materializeQueueAttributes(queue storage.Queue) map[string]string {
	attrs := cloneMap(queue.Attributes)
	attrs["QueueArn"] = queue.ARN
	attrs["CreatedTimestamp"] = strconvFormat(queue.CreatedAt.Unix())
	attrs["LastModifiedTimestamp"] = strconvFormat(queue.LastModifiedAt.Unix())
	attrs["ApproximateNumberOfMessages"] = "0"
	attrs["ApproximateNumberOfMessagesNotVisible"] = "0"
	attrs["ApproximateNumberOfMessagesDelayed"] = "0"
	return attrs
}

func onlyRetention(attrs map[string]string) bool {
	return len(attrs) == 1 && attrs["MessageRetentionPeriod"] != ""
}

func intBetween(raw string, min int64, max int64) bool {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return false
	}
	return value >= min && value <= max
}

func toAnySlice(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func stringMapToAny(values map[string]string) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func strconvFormat(value int64) string {
	return fmt.Sprintf("%d", value)
}
