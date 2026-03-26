package protocol

import (
	"context"
	"net/http"
	"time"
)

type WireProtocol string

const (
	WireProtocolJSON  WireProtocol = "json"
	WireProtocolQuery WireProtocol = "query"
)

type Identity struct {
	AccessKeyID  string
	AccountID    string
	PrincipalARN string
	PrincipalID  string
	SessionToken string
	Region       string
	Service      string
}

type Request struct {
	Context       context.Context
	WireProtocol  WireProtocol
	Operation     string
	Input         map[string]any
	HTTPRequest   *http.Request
	RequestID     string
	ReceivedAt    time.Time
	Identity      Identity
	QueuePathHint string
}

type Response struct {
	Output  map[string]any
	Headers map[string]string
}

type Dispatcher interface {
	Dispatch(Request) (Response, error)
}
