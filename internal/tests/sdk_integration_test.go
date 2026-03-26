package tests

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqssdk "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func TestSDKMessagePlaneBaseEndpoint(t *testing.T) {
	ctx := context.Background()
	server := startIntegrationServer(t)
	client := newSDKClientBaseEndpoint(t, server.baseURL)

	createOut, err := client.CreateQueue(ctx, &sqssdk.CreateQueueInput{
		QueueName: aws.String("sdk-standard"),
		Attributes: map[string]string{
			"VisibilityTimeout": "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	queueURL := aws.ToString(createOut.QueueUrl)

	sendOut, err := client.SendMessage(ctx, &sqssdk.SendMessageInput{
		QueueUrl:    aws.String(queueURL),
		MessageBody: aws.String("one"),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"Flavor": {
				DataType:    aws.String("String"),
				StringValue: aws.String("vanilla"),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(sendOut.MD5OfMessageBody) == "" || aws.ToString(sendOut.MessageId) == "" {
		t.Fatalf("unexpected send output: %#v", sendOut)
	}

	receiveOut, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:                    aws.String(queueURL),
		MaxNumberOfMessages:         1,
		VisibilityTimeout:           1,
		MessageAttributeNames:       []string{"All"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receiveOut.Messages) != 1 {
		t.Fatalf("expected one message, got %#v", receiveOut.Messages)
	}
	msg := receiveOut.Messages[0]
	firstHandle := aws.ToString(msg.ReceiptHandle)
	if msg.MessageAttributes["Flavor"].StringValue == nil || aws.ToString(msg.MessageAttributes["Flavor"].StringValue) != "vanilla" {
		t.Fatalf("missing message attributes: %#v", msg)
	}
	if got := msg.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)]; got != "1" {
		t.Fatalf("unexpected receive count: %#v", msg.Attributes)
	}

	if _, err := client.ChangeMessageVisibility(ctx, &sqssdk.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(queueURL),
		ReceiptHandle:     aws.String(firstHandle),
		VisibilityTimeout: 0,
	}); err != nil {
		t.Fatal(err)
	}

	receiveOut, err = client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:                    aws.String(queueURL),
		MaxNumberOfMessages:         1,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receiveOut.Messages) != 1 {
		t.Fatalf("expected one message after visibility reset, got %#v", receiveOut.Messages)
	}
	secondHandle := aws.ToString(receiveOut.Messages[0].ReceiptHandle)
	if secondHandle == firstHandle {
		t.Fatalf("expected a new receipt handle, got %q", secondHandle)
	}

	if _, err := client.DeleteMessage(ctx, &sqssdk.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: aws.String(firstHandle),
	}); err != nil {
		t.Fatalf("stale delete should succeed: %v", err)
	}
	if _, err := client.DeleteMessage(ctx, &sqssdk.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: aws.String(secondHandle),
	}); err != nil {
		t.Fatal(err)
	}

	emptyOut, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyOut.Messages) != 0 {
		t.Fatalf("expected queue empty after delete, got %#v", emptyOut.Messages)
	}

	delayOut, err := client.SendMessageBatch(ctx, &sqssdk.SendMessageBatchInput{
		QueueUrl: aws.String(queueURL),
		Entries: []types.SendMessageBatchRequestEntry{
			{
				Id:          aws.String("a"),
				MessageBody: aws.String("immediate"),
			},
			{
				Id:           aws.String("b"),
				MessageBody:  aws.String("delayed"),
				DelaySeconds: 1,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(delayOut.Successful) != 2 || len(delayOut.Failed) != 0 {
		t.Fatalf("unexpected batch send output: %#v", delayOut)
	}

	receiveOut, err = client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 10,
		VisibilityTimeout:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receiveOut.Messages) != 1 || aws.ToString(receiveOut.Messages[0].Body) != "immediate" {
		t.Fatalf("expected only immediate message, got %#v", receiveOut.Messages)
	}

	deleteBatchOut, err := client.DeleteMessageBatch(ctx, &sqssdk.DeleteMessageBatchInput{
		QueueUrl: aws.String(queueURL),
		Entries: []types.DeleteMessageBatchRequestEntry{
			{
				Id:            aws.String("good"),
				ReceiptHandle: receiveOut.Messages[0].ReceiptHandle,
			},
			{
				Id:            aws.String("bad"),
				ReceiptHandle: aws.String("bogus"),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleteBatchOut.Successful) != 1 || len(deleteBatchOut.Failed) != 1 {
		t.Fatalf("expected mixed delete batch result, got %#v", deleteBatchOut)
	}

	time.Sleep(1100 * time.Millisecond)
	receiveOut, err = client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 10,
		VisibilityTimeout:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receiveOut.Messages) != 1 || aws.ToString(receiveOut.Messages[0].Body) != "delayed" {
		t.Fatalf("expected delayed message after wait, got %#v", receiveOut.Messages)
	}

	changeBatchOut, err := client.ChangeMessageVisibilityBatch(ctx, &sqssdk.ChangeMessageVisibilityBatchInput{
		QueueUrl: aws.String(queueURL),
		Entries: []types.ChangeMessageVisibilityBatchRequestEntry{
			{
				Id:                aws.String("good"),
				ReceiptHandle:     receiveOut.Messages[0].ReceiptHandle,
				VisibilityTimeout: 0,
			},
			{
				Id:                aws.String("bad"),
				ReceiptHandle:     aws.String("bogus"),
				VisibilityTimeout: 0,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changeBatchOut.Successful) != 1 || len(changeBatchOut.Failed) != 1 {
		t.Fatalf("expected mixed visibility batch result, got %#v", changeBatchOut)
	}

	start := time.Now()
	emptyOut, err = client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyOut.Messages) != 1 {
		t.Fatalf("expected message to reappear after visibility reset, got %#v", emptyOut.Messages)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("expected immediate return when message available, got %v", time.Since(start))
	}
}

func TestSDKFIFOAndDLQResolverV2(t *testing.T) {
	ctx := context.Background()
	server := startIntegrationServer(t)
	client := newSDKClientResolverV2(t, server.baseURL)

	fifoOut, err := client.CreateQueue(ctx, &sqssdk.CreateQueueInput{
		QueueName: aws.String("sdk-fifo.fifo"),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "false",
			"VisibilityTimeout":         "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fifoURL := aws.ToString(fifoOut.QueueUrl)

	sendOne, err := client.SendMessage(ctx, &sqssdk.SendMessageInput{
		QueueUrl:               aws.String(fifoURL),
		MessageBody:            aws.String("first"),
		MessageGroupId:         aws.String("group-1"),
		MessageDeduplicationId: aws.String("dedup-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	sendTwo, err := client.SendMessage(ctx, &sqssdk.SendMessageInput{
		QueueUrl:               aws.String(fifoURL),
		MessageBody:            aws.String("second"),
		MessageGroupId:         aws.String("group-1"),
		MessageDeduplicationId: aws.String("dedup-2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(sendTwo.MessageId) == "" {
		t.Fatalf("expected second fifo send message id")
	}
	sendDup, err := client.SendMessage(ctx, &sqssdk.SendMessageInput{
		QueueUrl:               aws.String(fifoURL),
		MessageBody:            aws.String("first"),
		MessageGroupId:         aws.String("group-1"),
		MessageDeduplicationId: aws.String("dedup-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(sendOne.MessageId) != aws.ToString(sendDup.MessageId) {
		t.Fatalf("expected deduplicated send to replay original response, got %#v vs %#v", sendOne, sendDup)
	}

	recv, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:                    aws.String(fifoURL),
		MaxNumberOfMessages:         10,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
		ReceiveRequestAttemptId:     aws.String("attempt-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recv.Messages) != 2 {
		t.Fatalf("expected 2 FIFO messages, got %#v", recv.Messages)
	}
	if aws.ToString(recv.Messages[0].Body) != "first" || aws.ToString(recv.Messages[1].Body) != "second" {
		t.Fatalf("unexpected FIFO ordering: %#v", recv.Messages)
	}

	replayed, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:                    aws.String(fifoURL),
		MaxNumberOfMessages:         10,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
		ReceiveRequestAttemptId:     aws.String("attempt-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Messages) != 2 || aws.ToString(replayed.Messages[0].ReceiptHandle) != aws.ToString(recv.Messages[0].ReceiptHandle) {
		t.Fatalf("expected receive attempt replay, got %#v", replayed.Messages)
	}

	_, err = client.SendMessage(ctx, &sqssdk.SendMessageInput{
		QueueUrl:               aws.String(fifoURL),
		MessageBody:            aws.String("delayed"),
		MessageGroupId:         aws.String("group-2"),
		MessageDeduplicationId: aws.String("dedup-delay"),
		DelaySeconds:           1,
	})
	if err == nil {
		t.Fatal("expected per-message delay on FIFO to be rejected")
	}

	contentFIFO, err := client.CreateQueue(ctx, &sqssdk.CreateQueueInput{
		QueueName: aws.String("sdk-content.fifo"),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	contentURL := aws.ToString(contentFIFO.QueueUrl)
	if _, err := client.SendMessage(ctx, &sqssdk.SendMessageInput{
		QueueUrl:       aws.String(contentURL),
		MessageBody:    aws.String("same-body"),
		MessageGroupId: aws.String("group-1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendMessage(ctx, &sqssdk.SendMessageInput{
		QueueUrl:       aws.String(contentURL),
		MessageBody:    aws.String("same-body"),
		MessageGroupId: aws.String("group-1"),
	}); err != nil {
		t.Fatal(err)
	}
	contentRecv, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:            aws.String(contentURL),
		MaxNumberOfMessages: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(contentRecv.Messages) != 1 {
		t.Fatalf("expected content-based dedup to collapse duplicate messages, got %#v", contentRecv.Messages)
	}

	dlqOut, err := client.CreateQueue(ctx, &sqssdk.CreateQueueInput{
		QueueName: aws.String("sdk-dlq"),
		Attributes: map[string]string{
			"VisibilityTimeout": "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	dlqURL := aws.ToString(dlqOut.QueueUrl)
	attrOut, err := client.GetQueueAttributes(ctx, &sqssdk.GetQueueAttributesInput{
		QueueUrl:       aws.String(dlqURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatal(err)
	}
	dlqARN := attrOut.Attributes[string(types.QueueAttributeNameQueueArn)]

	sourcePolicy, _ := json.Marshal(map[string]any{
		"deadLetterTargetArn": dlqARN,
		"maxReceiveCount":     "1",
	})
	sourceOut, err := client.CreateQueue(ctx, &sqssdk.CreateQueueInput{
		QueueName: aws.String("sdk-source"),
		Attributes: map[string]string{
			"VisibilityTimeout": "1",
			"RedrivePolicy":     string(sourcePolicy),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceURL := aws.ToString(sourceOut.QueueUrl)

	if _, err := client.SendMessage(ctx, &sqssdk.SendMessageInput{
		QueueUrl:    aws.String(sourceURL),
		MessageBody: aws.String("to-dlq"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:            aws.String(sourceURL),
		MaxNumberOfMessages: 1,
		VisibilityTimeout:   1,
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1100 * time.Millisecond)
	sourceRecv, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:            aws.String(sourceURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceRecv.Messages) != 0 {
		t.Fatalf("expected source queue empty after redrive, got %#v", sourceRecv.Messages)
	}

	dlqRecv, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:                    aws.String(dlqURL),
		MaxNumberOfMessages:         1,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dlqRecv.Messages) != 1 {
		t.Fatalf("expected message in DLQ, got %#v", dlqRecv.Messages)
	}
	if got := dlqRecv.Messages[0].Attributes[string(types.MessageSystemAttributeNameDeadLetterQueueSourceArn)]; got != sourceURLToARN(sourceURL) {
		t.Fatalf("unexpected DLQ source ARN: %#v", dlqRecv.Messages[0].Attributes)
	}

	moveOut, err := client.StartMessageMoveTask(ctx, &sqssdk.StartMessageMoveTaskInput{
		SourceArn: aws.String(dlqARN),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(moveOut.TaskHandle) == "" {
		t.Fatalf("expected task handle in move task output")
	}

	var moveTasks *sqssdk.ListMessageMoveTasksOutput
	for range 10 {
		moveTasks, err = client.ListMessageMoveTasks(ctx, &sqssdk.ListMessageMoveTasksInput{
			SourceArn:  aws.String(dlqARN),
			MaxResults: aws.Int32(10),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(moveTasks.Results) != 0 && aws.ToString(moveTasks.Results[0].Status) == "COMPLETED" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if moveTasks == nil || len(moveTasks.Results) == 0 {
		t.Fatalf("expected move task listing")
	}

	sourceRecv, err = client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:            aws.String(sourceURL),
		MaxNumberOfMessages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceRecv.Messages) != 1 || aws.ToString(sourceRecv.Messages[0].Body) != "to-dlq" {
		t.Fatalf("expected message moved back to source queue, got %#v", sourceRecv.Messages)
	}
}

func sourceURLToARN(queueURL string) string {
	parts := strings.Split(strings.TrimPrefix(queueURL, "http://"), "/")
	accountID := parts[len(parts)-2]
	name := parts[len(parts)-1]
	return "arn:aws:sqs:" + testRegion + ":" + accountID + ":" + name
}

func TestSDKLongPollOnEmptyQueue(t *testing.T) {
	ctx := context.Background()
	server := startIntegrationServer(t)
	client := newSDKClientBaseEndpoint(t, server.baseURL)

	createOut, err := client.CreateQueue(ctx, &sqssdk.CreateQueueInput{
		QueueName: aws.String("sdk-long-poll"),
	})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, err := client.ReceiveMessage(ctx, &sqssdk.ReceiveMessageInput{
		QueueUrl:            createOut.QueueUrl,
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 0 {
		t.Fatalf("expected no messages, got %#v", out.Messages)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("expected long poll wait, got %v", time.Since(start))
	}
}

func TestSDKBatchErrorSurface(t *testing.T) {
	ctx := context.Background()
	server := startIntegrationServer(t)
	client := newSDKClientBaseEndpoint(t, server.baseURL)

	createOut, err := client.CreateQueue(ctx, &sqssdk.CreateQueueInput{
		QueueName: aws.String("sdk-batch-errors"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.SendMessageBatch(ctx, &sqssdk.SendMessageBatchInput{
		QueueUrl: createOut.QueueUrl,
		Entries: []types.SendMessageBatchRequestEntry{
			{Id: aws.String("dup"), MessageBody: aws.String("one")},
			{Id: aws.String("dup"), MessageBody: aws.String("two")},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate batch ids to fail")
	}
	if !strings.Contains(err.Error(), "BatchEntryIdsNotDistinct") {
		t.Fatalf("unexpected batch error: %v", err)
	}
}
