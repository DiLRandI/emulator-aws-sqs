package httpx

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"emulator-aws-sqs/internal/auth"
	"emulator-aws-sqs/internal/clock"
	apierrors "emulator-aws-sqs/internal/errors"
	"emulator-aws-sqs/internal/model"
	"emulator-aws-sqs/internal/protocol"
	jsonproto "emulator-aws-sqs/internal/protocol/json"
	queryproto "emulator-aws-sqs/internal/protocol/query"
)

type Handler struct {
	Registry   model.Registry
	Clock      clock.Clock
	Auth       auth.Authenticator
	Dispatcher protocol.Dispatcher
	Logger     *slog.Logger
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := NewRequestID()
	receivedAt := h.Clock.Now()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeJSONError(w, requestID, apierrors.Internal("failed to read request body", err))
		return
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	wire := detectWireProtocol(r, body)
	identity, err := h.Auth.Authenticate(r.Context(), r, body)
	if err != nil {
		h.log("auth failed", requestID, wire, "", err)
		h.writeError(w, wire, requestID, err)
		return
	}

	var (
		action string
		input  map[string]any
	)

	switch wire {
	case protocol.WireProtocolJSON:
		action, input, err = jsonproto.DecodeRequest(h.Registry, r, body)
	case protocol.WireProtocolQuery:
		action, input, err = queryproto.DecodeRequest(h.Registry, r, body)
	default:
		err = apierrors.ErrUnsupportedOperation
	}
	if err != nil {
		h.log("decode failed", requestID, wire, action, err)
		h.writeError(w, wire, requestID, err)
		return
	}

	resp, err := h.Dispatcher.Dispatch(protocol.Request{
		Context:       r.Context(),
		WireProtocol:  wire,
		Operation:     action,
		Input:         input,
		HTTPRequest:   r,
		RequestID:     requestID,
		ReceivedAt:    receivedAt,
		Identity:      identity,
		QueuePathHint: strings.TrimPrefix(r.URL.Path, "/"),
	})
	if err != nil {
		h.log("dispatch failed", requestID, wire, action, err)
		h.writeError(w, wire, requestID, err)
		return
	}

	switch wire {
	case protocol.WireProtocolJSON:
		status, headers, payload, err := jsonproto.EncodeResponse(h.Registry, action, requestID, resp)
		if err != nil {
			h.writeJSONError(w, requestID, apierrors.Internal("failed to encode response", err))
			return
		}
		writeResponse(w, status, headers, payload)
	case protocol.WireProtocolQuery:
		status, headers, payload, err := queryproto.EncodeResponse(h.Registry, action, requestID, resp)
		if err != nil {
			h.writeJSONError(w, requestID, apierrors.Internal("failed to encode response", err))
			return
		}
		writeResponse(w, status, headers, payload)
	}
}

func detectWireProtocol(r *http.Request, body []byte) protocol.WireProtocol {
	if strings.HasPrefix(r.Header.Get("X-Amz-Target"), "AmazonSQS.") {
		return protocol.WireProtocolJSON
	}
	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/x-amz-json-1.0") {
		return protocol.WireProtocolJSON
	}
	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		return protocol.WireProtocolQuery
	}
	if bytes.Contains(body, []byte("Action=")) || r.URL.Query().Get("Action") != "" {
		return protocol.WireProtocolQuery
	}
	return protocol.WireProtocolJSON
}

func (h Handler) writeError(w http.ResponseWriter, wire protocol.WireProtocol, requestID string, err error) {
	switch wire {
	case protocol.WireProtocolQuery:
		status, headers, payload := queryproto.EncodeError(requestID, err)
		writeResponse(w, status, headers, payload)
	default:
		h.writeJSONError(w, requestID, err)
	}
}

func (h Handler) writeJSONError(w http.ResponseWriter, requestID string, err error) {
	status, headers, payload := jsonproto.EncodeError(requestID, err)
	writeResponse(w, status, headers, payload)
}

func writeResponse(w http.ResponseWriter, status int, headers http.Header, payload []byte) {
	for key, values := range headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(status)
	if len(payload) != 0 {
		_, _ = w.Write(payload)
	}
}

func (h Handler) log(message, requestID string, wire protocol.WireProtocol, action string, err error) {
	if h.Logger == nil {
		return
	}
	h.Logger.Error(message, "request_id", requestID, "wire_protocol", wire, "action", action, "error", err)
}
