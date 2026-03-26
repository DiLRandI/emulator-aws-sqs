package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRawJSONLifecycleSigned(t *testing.T) {
	server := startIntegrationServer(t)

	var create map[string]string
	resp := doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "CreateQueue", map[string]any{
		"QueueName": "raw-json",
		"Attributes": map[string]string{
			"VisibilityTimeout": "1",
		},
	}), &create)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create queue status=%d", resp.StatusCode)
	}
	queueURL := create["QueueUrl"]

	var send map[string]any
	resp = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "SendMessage", map[string]any{
		"QueueUrl":    queueURL,
		"MessageBody": "hello",
		"MessageAttributes": map[string]any{
			"Flavor": map[string]any{
				"DataType":    "String",
				"StringValue": "vanilla",
			},
		},
		"MessageSystemAttributes": map[string]any{
			"AWSTraceHeader": map[string]any{
				"DataType":    "String",
				"StringValue": "Root=1-12345678-abcdef012345678912345678",
			},
		},
	}), &send)
	if got := resp.Header.Get("Content-Type"); got != "application/x-amz-json-1.0" {
		t.Fatalf("unexpected content type: %s", got)
	}
	if _, ok := send["MessageId"]; !ok {
		t.Fatalf("missing MessageId: %#v", send)
	}
	if _, ok := send["MD5OfMessageBody"]; !ok {
		t.Fatalf("missing MD5OfMessageBody: %#v", send)
	}

	var receive struct {
		Messages []map[string]any `json:"Messages"`
	}
	resp = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "ReceiveMessage", map[string]any{
		"QueueUrl":                    queueURL,
		"WaitTimeSeconds":             0,
		"VisibilityTimeout":           1,
		"MessageAttributeNames":       []string{"All"},
		"MessageSystemAttributeNames": []string{"All"},
	}), &receive)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("receive status=%d", resp.StatusCode)
	}
	if len(receive.Messages) != 1 {
		t.Fatalf("expected one message, got %#v", receive.Messages)
	}
	firstHandle := receive.Messages[0]["ReceiptHandle"].(string)
	attrs := receive.Messages[0]["Attributes"].(map[string]any)
	if attrs["ApproximateReceiveCount"] != "1" {
		t.Fatalf("unexpected receive count: %#v", attrs)
	}
	if _, ok := receive.Messages[0]["MessageAttributes"].(map[string]any)["Flavor"]; !ok {
		t.Fatalf("expected message attribute in receive: %#v", receive.Messages[0])
	}

	resp = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "ChangeMessageVisibility", map[string]any{
		"QueueUrl":          queueURL,
		"ReceiptHandle":     firstHandle,
		"VisibilityTimeout": 0,
	}), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("change visibility status=%d", resp.StatusCode)
	}

	receive = struct {
		Messages []map[string]any `json:"Messages"`
	}{}
	resp = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "ReceiveMessage", map[string]any{
		"QueueUrl":                    queueURL,
		"WaitTimeSeconds":             0,
		"MessageSystemAttributeNames": []string{"All"},
	}), &receive)
	if len(receive.Messages) != 1 {
		t.Fatalf("expected one message after visibility reset, got %#v", receive.Messages)
	}
	secondHandle := receive.Messages[0]["ReceiptHandle"].(string)
	if secondHandle == firstHandle {
		t.Fatalf("expected new receipt handle, got same handle %q", secondHandle)
	}

	resp = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "DeleteMessage", map[string]any{
		"QueueUrl":      queueURL,
		"ReceiptHandle": firstHandle,
	}), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete with stale handle should succeed, got %d", resp.StatusCode)
	}

	resp = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "DeleteMessage", map[string]any{
		"QueueUrl":      queueURL,
		"ReceiptHandle": secondHandle,
	}), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete with current handle failed: %d", resp.StatusCode)
	}

	receive = struct {
		Messages []map[string]any `json:"Messages"`
	}{}
	_ = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "ReceiveMessage", map[string]any{
		"QueueUrl":        queueURL,
		"WaitTimeSeconds": 0,
	}), &receive)
	if len(receive.Messages) != 0 {
		t.Fatalf("expected queue empty after delete, got %#v", receive.Messages)
	}
}

func TestRawQueryBatchFIFOAndLongPoll(t *testing.T) {
	server := startIntegrationServer(t)

	var create map[string]string
	_ = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "CreateQueue", map[string]any{
		"QueueName": "raw-fifo.fifo",
		"Attributes": map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "true",
			"VisibilityTimeout":         "1",
		},
	}), &create)
	queueURL := create["QueueUrl"]
	queuePath := strings.TrimPrefix(queueURL, server.baseURL)

	values := url.Values{}
	values.Set("Action", "SendMessage")
	values.Set("MessageBody", "fifo-one")
	values.Set("MessageGroupId", "g1")
	values.Set("DelaySeconds", "1")
	req := signedQueryRequest(t, http.MethodPost, server.baseURL+queuePath, values)
	resp, err := server.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected delay rejection on fifo queue, got %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/xml" {
		t.Fatalf("unexpected query error content type: %s", got)
	}
	if !bytes.Contains(body, []byte("<Code>InvalidParameterValue</Code>")) {
		t.Fatalf("expected InvalidParameterValue, got %s", body)
	}

	values = url.Values{}
	values.Set("Action", "SendMessageBatch")
	values.Set("SendMessageBatchRequestEntry.1.Id", "a")
	values.Set("SendMessageBatchRequestEntry.1.MessageBody", "first")
	values.Set("SendMessageBatchRequestEntry.1.MessageGroupId", "g1")
	values.Set("SendMessageBatchRequestEntry.2.Id", "b")
	values.Set("SendMessageBatchRequestEntry.2.MessageBody", "second")
	values.Set("SendMessageBatchRequestEntry.2.MessageGroupId", "g1")
	req = signedQueryRequest(t, http.MethodPost, server.baseURL+queuePath, values)
	resp, err = server.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send batch status=%d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("<SendMessageBatchResponse xmlns=\"http://queue.amazonaws.com/doc/2012-11-05/\">")) {
		t.Fatalf("missing expected XML namespace: %s", body)
	}

	values = url.Values{}
	values.Set("Action", "ReceiveMessage")
	values.Set("MaxNumberOfMessages", "10")
	values.Set("MessageSystemAttributeName.1", "All")
	req = signedQueryRequest(t, http.MethodPost, server.baseURL+queuePath, values)
	resp, err = server.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	receivedXML := append([]byte(nil), body...)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("receive query status=%d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("<MessageId>")) || !bytes.Contains(body, []byte("<Attribute><Name>SequenceNumber</Name>")) {
		t.Fatalf("expected message payload and Attribute envelope, got %s", body)
	}
	firstIdx := bytes.Index(body, []byte("<Body>first</Body>"))
	secondIdx := bytes.Index(body, []byte("<Body>second</Body>"))
	if firstIdx < 0 || secondIdx < 0 || firstIdx > secondIdx {
		t.Fatalf("expected FIFO order in query response: %s", body)
	}

	values = url.Values{}
	values.Set("Action", "ReceiveMessage")
	values.Set("WaitTimeSeconds", "1")
	req = signedQueryRequest(t, http.MethodPost, server.baseURL+queuePath, values)
	start := time.Now()
	resp, err = server.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("expected long poll to wait, got %v body=%s", time.Since(start), body)
	}

	values = url.Values{}
	values.Set("Action", "DeleteMessageBatch")
	values.Set("DeleteMessageBatchRequestEntry.1.Id", "good")
	values.Set("DeleteMessageBatchRequestEntry.1.ReceiptHandle", extractXMLValue(t, bodyString(receivedXML), "ReceiptHandle"))
	values.Set("DeleteMessageBatchRequestEntry.2.Id", "bad")
	values.Set("DeleteMessageBatchRequestEntry.2.ReceiptHandle", "bogus")
	req = signedQueryRequest(t, http.MethodPost, server.baseURL+queuePath, values)
	resp, err = server.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete batch status=%d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("<DeleteMessageBatchResultEntry><Id>good</Id></DeleteMessageBatchResultEntry>")) ||
		!bytes.Contains(body, []byte("<BatchResultErrorEntry><Code>ReceiptHandleIsInvalid</Code>")) {
		t.Fatalf("expected mixed batch result, got %s", body)
	}
}

func TestRawJSONBatchOperations(t *testing.T) {
	server := startIntegrationServer(t)

	var create map[string]string
	_ = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "CreateQueue", map[string]any{
		"QueueName": "raw-json-batch",
		"Attributes": map[string]string{
			"VisibilityTimeout": "5",
		},
	}), &create)
	queueURL := create["QueueUrl"]

	var sendBatch map[string]any
	resp := doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "SendMessageBatch", map[string]any{
		"QueueUrl": queueURL,
		"Entries": []map[string]any{
			{
				"Id":          "a",
				"MessageBody": "one",
			},
			{
				"Id":          "b",
				"MessageBody": "two",
			},
		},
	}), &sendBatch)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send batch status=%d body=%v", resp.StatusCode, sendBatch)
	}
	if len(sendBatch["Successful"].([]any)) != 2 {
		t.Fatalf("unexpected send batch output: %#v", sendBatch)
	}

	var receive struct {
		Messages []map[string]any `json:"Messages"`
	}
	_ = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "ReceiveMessage", map[string]any{
		"QueueUrl":            queueURL,
		"MaxNumberOfMessages": 10,
		"VisibilityTimeout":   5,
	}), &receive)
	if len(receive.Messages) != 2 {
		t.Fatalf("expected two received messages, got %#v", receive.Messages)
	}

	var changeBatch map[string]any
	_ = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "ChangeMessageVisibilityBatch", map[string]any{
		"QueueUrl": queueURL,
		"Entries": []map[string]any{
			{
				"Id":                "good",
				"ReceiptHandle":     receive.Messages[0]["ReceiptHandle"],
				"VisibilityTimeout": 0,
			},
			{
				"Id":                "bad",
				"ReceiptHandle":     "bogus",
				"VisibilityTimeout": 0,
			},
		},
	}), &changeBatch)
	if len(changeBatch["Successful"].([]any)) != 1 || len(changeBatch["Failed"].([]any)) != 1 {
		t.Fatalf("unexpected change visibility batch output: %#v", changeBatch)
	}

	var deleteBatch map[string]any
	_ = doJSON(t, server.client, signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "DeleteMessageBatch", map[string]any{
		"QueueUrl": queueURL,
		"Entries": []map[string]any{
			{
				"Id":            "good",
				"ReceiptHandle": receive.Messages[1]["ReceiptHandle"],
			},
			{
				"Id":            "bad",
				"ReceiptHandle": "bogus",
			},
		},
	}), &deleteBatch)
	if len(deleteBatch["Successful"].([]any)) != 1 || len(deleteBatch["Failed"].([]any)) != 1 {
		t.Fatalf("unexpected delete batch output: %#v", deleteBatch)
	}
}

func TestRawUnsignedRejected(t *testing.T) {
	server := startIntegrationServer(t)
	req, err := http.NewRequest(http.MethodPost, server.baseURL+"/", strings.NewReader(`{"QueueName":"unsigned"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS.CreateQueue")
	resp, err := server.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected auth failure, got %d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("InvalidSecurity")) {
		t.Fatalf("expected InvalidSecurity body, got %s", body)
	}
}

func bodyString(raw []byte) string {
	return string(raw)
}

func extractXMLValue(t *testing.T, raw, tag string) string {
	t.Helper()
	start := strings.Index(raw, "<"+tag+">")
	if start < 0 {
		t.Fatalf("missing tag %s in %s", tag, raw)
	}
	start += len(tag) + 2
	end := strings.Index(raw[start:], "</"+tag+">")
	if end < 0 {
		t.Fatalf("missing closing tag %s in %s", tag, raw)
	}
	return raw[start : start+end]
}

func TestRawJSONProtocolErrorEnvelope(t *testing.T) {
	server := startIntegrationServer(t)
	req := signedJSONRequest(t, http.MethodPost, server.baseURL+"/", "SendMessage", map[string]any{
		"QueueUrl":    server.baseURL + "/000000000000/missing",
		"MessageBody": "oops",
	})
	resp, err := server.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := resp.Header.Get("Content-Type"); got != "application/x-amz-json-1.0" {
		t.Fatalf("unexpected content type: %s", got)
	}
	if got := resp.Header.Get("x-amzn-RequestId"); got == "" {
		t.Fatalf("missing request id header")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["__type"] != "com.amazonaws.sqs#QueueDoesNotExist" {
		t.Fatalf("unexpected json error payload: %#v", payload)
	}
}
