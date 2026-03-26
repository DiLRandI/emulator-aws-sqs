package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"emulator-aws-sqs/internal/auth"
	"emulator-aws-sqs/internal/clock"
	"emulator-aws-sqs/internal/config"
	"emulator-aws-sqs/internal/httpx"
	"emulator-aws-sqs/internal/model"
	"emulator-aws-sqs/internal/service"
	sqlitestore "emulator-aws-sqs/internal/storage/sqlite"
)

func TestCreateQueueJSONAndListQuery(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	createPayload := map[string]any{
		"QueueName": "test-queue",
		"Attributes": map[string]string{
			"VisibilityTimeout": "45",
		},
	}
	createBody, err := json.Marshal(createPayload)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(createBody))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS.CreateQueue")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var createResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &createResp); err != nil {
		t.Fatal(err)
	}
	queueURL := createResp["QueueUrl"].(string)

	form := url.Values{}
	form.Set("Action", "ListQueues")
	form.Set("QueueNamePrefix", "test")
	queryBody := []byte(form.Encode())
	req = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(queryBody))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected query status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("<QueueUrl>"+queueURL+"</QueueUrl>")) {
		t.Fatalf("expected queue URL in query response: %s", rec.Body.String())
	}
}

func TestSetAndGetQueueAttributesJSON(t *testing.T) {
	handler, cleanup := newTestHandler(t)
	defer cleanup()

	queueURL := createQueue(t, handler, "attrs-queue")

	updatePayload := map[string]any{
		"QueueUrl": queueURL,
		"Attributes": map[string]string{
			"VisibilityTimeout": "60",
		},
	}
	updateBody, err := json.Marshal(updatePayload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(updateBody))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS.SetQueueAttributes")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	getPayload := map[string]any{
		"QueueUrl":       queueURL,
		"AttributeNames": []string{"VisibilityTimeout"},
	}
	getBody, err := json.Marshal(getPayload)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(getBody))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS.GetQueueAttributes")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var getResp map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &getResp); err != nil {
		t.Fatal(err)
	}
	if got := getResp["Attributes"]["VisibilityTimeout"]; got != "30" {
		t.Fatalf("expected pending attribute to remain unpropagated immediately, got %q", got)
	}
}

func createQueue(t *testing.T, handler http.Handler, name string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"QueueName": name})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS.CreateQueue")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out["QueueUrl"]
}

func newTestHandler(t *testing.T) (http.Handler, func()) {
	t.Helper()
	store, err := sqlitestore.Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		ListenAddr:           ":0",
		PublicBaseURL:        "http://127.0.0.1:9324",
		Region:               "us-east-1",
		AllowedRegions:       []string{"us-east-1"},
		AccountID:            "000000000000",
		AuthMode:             "bypass",
		CreatePropagation:    0,
		DeleteCooldown:       60 * time.Second,
		PurgeCooldown:        60 * time.Second,
		AttributePropagation: 60 * time.Second,
		RetentionPropagation: 15 * time.Minute,
	}

	registry := auth.NewRegistry(cfg.AccountID)
	clk := clock.RealClock{}
	handler := httpx.Handler{
		Registry:   model.Default,
		Clock:      clk,
		Auth:       auth.NewService(cfg.AuthMode, clk, registry, cfg.AllowedRegions, cfg.Region, cfg.AccountID),
		Dispatcher: service.NewSQS(store, clk, cfg),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return handler, func() { _ = store.Close() }
}
