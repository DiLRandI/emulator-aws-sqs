package errors

import (
	stderrors "errors"
	"fmt"
	"net/http"
)

type FaultType string

const (
	SenderFault   FaultType = "Sender"
	ReceiverFault FaultType = "Receiver"
)

type APIError struct {
	Code           string
	Message        string
	HTTPStatusCode int
	Fault          FaultType
	QueryErrorCode string
	Detail         string
	Cause          error
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
}

func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *APIError) WithCause(err error) *APIError {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Cause = err
	return &clone
}

func New(code string, status int, message string) *APIError {
	return &APIError{
		Code:           code,
		Message:        message,
		HTTPStatusCode: status,
		Fault:          SenderFault,
		QueryErrorCode: "AWS.SimpleQueueService." + code + ";Sender",
	}
}

func Internal(message string, err error) *APIError {
	return &APIError{
		Code:           "InternalError",
		Message:        message,
		HTTPStatusCode: http.StatusInternalServerError,
		Fault:          ReceiverFault,
		QueryErrorCode: "AWS.SimpleQueueService.InternalError;Receiver",
		Cause:          err,
	}
}

func As(err error) (*APIError, bool) {
	var apiErr *APIError
	if stderrors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

var (
	ErrInvalidSecurity           = New("InvalidSecurity", http.StatusBadRequest, "The request was not made over HTTPS or did not use SigV4 for signing.")
	ErrInvalidAddress            = New("InvalidAddress", http.StatusBadRequest, "The specified ID is invalid.")
	ErrQueueDoesNotExist         = New("QueueDoesNotExist", http.StatusBadRequest, "The specified queue does not exist.")
	ErrQueueDeletedRecently      = New("QueueDeletedRecently", http.StatusBadRequest, "You must wait 60 seconds after deleting a queue before you can create another queue with the same name.")
	ErrQueueNameExists           = New("QueueNameExists", http.StatusBadRequest, "A queue with this name already exists.")
	ErrRequestThrottled          = New("RequestThrottled", http.StatusBadRequest, "The request was denied due to request throttling.")
	ErrUnsupportedOperation      = New("UnsupportedOperation", http.StatusBadRequest, "Unsupported operation.")
	ErrInvalidAttributeName      = New("InvalidAttributeName", http.StatusBadRequest, "The specified attribute doesn't exist.")
	ErrInvalidAttributeValue     = New("InvalidAttributeValue", http.StatusBadRequest, "A queue attribute value is invalid.")
	ErrReceiptHandleIsInvalid    = New("ReceiptHandleIsInvalid", http.StatusBadRequest, "The specified receipt handle isn't valid.")
	ErrMessageNotInflight        = New("MessageNotInflight", http.StatusBadRequest, "The specified message isn't in flight.")
	ErrOverLimit                 = New("OverLimit", http.StatusBadRequest, "The specified action violates a limit.")
	ErrInvalidMessageContents    = New("InvalidMessageContents", http.StatusBadRequest, "The message contains characters outside the allowed set.")
	ErrTooManyEntriesInBatch     = New("TooManyEntriesInBatchRequest", http.StatusBadRequest, "The batch request contains more entries than permissible.")
	ErrEmptyBatchRequest         = New("EmptyBatchRequest", http.StatusBadRequest, "The batch request doesn't contain any entries.")
	ErrBatchEntryIDsNotDistinct  = New("BatchEntryIdsNotDistinct", http.StatusBadRequest, "Two or more batch entries in the request have the same Id.")
	ErrInvalidBatchEntryID       = New("InvalidBatchEntryId", http.StatusBadRequest, "The Id of a batch entry in a batch request doesn't abide by the specification.")
	ErrBatchRequestTooLong       = New("BatchRequestTooLong", http.StatusBadRequest, "The length of all the messages put together is more than the limit.")
	ErrPurgeQueueInProgress      = New("PurgeQueueInProgress", http.StatusBadRequest, "Only one PurgeQueue operation on a queue is allowed every 60 seconds.")
	ErrResourceNotFoundException = New("ResourceNotFoundException", http.StatusBadRequest, "The specified resource does not exist.")
	ErrKMSDisabled               = New("KmsDisabled", http.StatusBadRequest, "The request was denied due to request throttling.")
	ErrKMSInvalidState           = New("KmsInvalidState", http.StatusBadRequest, "The request was rejected because the specified resource is not in a valid state.")
	ErrKMSNotFound               = New("KmsNotFound", http.StatusBadRequest, "The specified resource does not exist.")
	ErrKMSOptInRequired          = New("KmsOptInRequired", http.StatusBadRequest, "The request was rejected because the specified key policy is not syntactically or semantically correct.")
	ErrKMSThrottled              = New("KmsThrottled", http.StatusBadRequest, "The request was denied due to request throttling.")
	ErrKMSAccessDenied           = New("KmsAccessDenied", http.StatusBadRequest, "The caller does not have required permissions to use the KMS key.")
	ErrKMSInvalidKeyUsage        = New("KmsInvalidKeyUsage", http.StatusBadRequest, "The caller requests a key that is not valid for the operation.")
)
