package json

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"net/http"

	apierrors "emulator-aws-sqs/internal/errors"
	"emulator-aws-sqs/internal/model"
	"emulator-aws-sqs/internal/protocol"
)

func DecodeRequest(reg model.Registry, r *http.Request, body []byte) (string, map[string]any, error) {
	target := r.Header.Get("X-Amz-Target")
	action, ok := reg.ActionFromTarget(target)
	if !ok {
		return "", nil, apierrors.ErrUnsupportedOperation.WithCause(fmt.Errorf("unknown target %q", target))
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return action, map[string]any{}, nil
	}
	decoder := stdjson.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return "", nil, apierrors.New("MalformedInput", http.StatusBadRequest, "The request body could not be parsed.").WithCause(err)
	}
	return action, normalizeNumbers(payload).(map[string]any), nil
}

func EncodeResponse(reg model.Registry, operation, requestID string, resp protocol.Response) (int, http.Header, []byte, error) {
	headers := http.Header{}
	headers.Set("Content-Type", "application/x-amz-json-1.0")
	headers.Set("x-amzn-RequestId", requestID)
	op := reg.MustOperation(operation)
	if op.OutputShape == "" || len(resp.Output) == 0 {
		return http.StatusOK, headers, nil, nil
	}
	body, err := stdjson.Marshal(resp.Output)
	if err != nil {
		return 0, nil, nil, err
	}
	return http.StatusOK, headers, body, nil
}

func EncodeError(requestID string, err error) (int, http.Header, []byte) {
	apiErr, ok := apierrors.As(err)
	if !ok {
		apiErr = apierrors.Internal("internal server error", err)
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/x-amz-json-1.0")
	headers.Set("x-amzn-RequestId", requestID)
	headers.Set("x-amzn-query-error", apiErr.QueryErrorCode)
	body, _ := stdjson.Marshal(map[string]any{
		"__type":  "com.amazonaws.sqs#" + apiErr.Code,
		"message": apiErr.Message,
	})
	return apiErr.HTTPStatusCode, headers, body
}

func normalizeNumbers(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, entry := range typed {
			out[key] = normalizeNumbers(entry)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, entry := range typed {
			out = append(out, normalizeNumbers(entry))
		}
		return out
	case stdjson.Number:
		if i64, err := typed.Int64(); err == nil {
			return i64
		}
		if f64, err := typed.Float64(); err == nil {
			return f64
		}
		return typed.String()
	default:
		return value
	}
}
