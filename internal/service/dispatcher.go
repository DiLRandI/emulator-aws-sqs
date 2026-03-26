package service

import (
	"context"

	apierrors "emulator-aws-sqs/internal/errors"
	"emulator-aws-sqs/internal/protocol"
)

type ActionHandler func(context.Context, protocol.Request) (protocol.Response, error)

type Dispatcher struct {
	handlers map[string]ActionHandler
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		handlers: map[string]ActionHandler{},
	}
}

func (d *Dispatcher) Register(action string, handler ActionHandler) {
	d.handlers[action] = handler
}

func (d *Dispatcher) Dispatch(req protocol.Request) (protocol.Response, error) {
	handler, ok := d.handlers[req.Operation]
	if !ok {
		return protocol.Response{}, apierrors.ErrUnsupportedOperation
	}
	return handler(req.Context, req)
}
