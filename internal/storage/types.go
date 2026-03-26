package storage

import (
	"context"
	"time"
)

type Queue struct {
	ID               int64
	AccountID        string
	Region           string
	Name             string
	URL              string
	ARN              string
	FIFO             bool
	Attributes       map[string]string
	CreatedAt        time.Time
	AvailableAt      time.Time
	LastModifiedAt   time.Time
	DeletedAt        *time.Time
	ReuseBlockedTill *time.Time
	PurgeRequestedAt *time.Time
	PurgeExpiresAt   *time.Time
}

type PendingAttributePatch struct {
	ID          int64
	QueueID     int64
	Attributes  map[string]string
	EffectiveAt time.Time
	SubmittedAt time.Time
}

type MoveTask struct {
	ID                                int64
	TaskHandle                        string
	SourceQueueID                     int64
	DestinationQueueID                *int64
	Status                            string
	MaxMessagesPerSecond              *int64
	ApproximateNumberOfMessagesMoved  int64
	ApproximateNumberOfMessagesToMove *int64
	FailureReason                     string
	StartedAt                         time.Time
	UpdatedAt                         time.Time
	CompletedAt                       *time.Time
}

type Message struct {
	RowID                 int64
	QueueID               int64
	MessageID             string
	Body                  string
	BodyMD5               string
	MessageAttributes     map[string]any
	SystemAttributes      map[string]any
	SentAt                time.Time
	OriginalSentAt        time.Time
	FirstReceivedAt       *time.Time
	ReceiveCount          int64
	AvailableAt           time.Time
	VisibilityDeadline    *time.Time
	RetentionDeadline     time.Time
	DeletedAt             *time.Time
	GroupID               string
	DedupID               string
	SequenceNumber        string
	DeadLetterSourceARN   string
	EncryptionKeyID       string
	ReceiveAttemptID      string
	LastReceiptHandle     string
	CurrentSourceQueueARN string
}

type Receipt struct {
	ID                 int64
	QueueID            int64
	MessageRowID       int64
	Handle             string
	IssuedAt           time.Time
	VisibilityDeadline time.Time
	Active             bool
	ReceiveAttemptID   string
}

type DedupEntry struct {
	ID        int64
	QueueID   int64
	ScopeKey  string
	DedupID   string
	Response  map[string]any
	ExpiresAt time.Time
}

type ReceiveAttempt struct {
	ID            int64
	QueueID       int64
	AttemptID     string
	Response      []map[string]any
	ExpiresAt     time.Time
	InvalidatedAt *time.Time
}

type QueueListPage struct {
	Queues    []Queue
	NextToken string
}

type Store interface {
	Init(context.Context) error
	Close() error

	ApplyDueQueueMutations(context.Context, time.Time) error
	InsertQueue(context.Context, Queue) (Queue, error)
	UpdateQueue(context.Context, Queue) error
	DeleteQueueRow(context.Context, int64) error
	QueueByID(context.Context, int64) (Queue, bool, error)
	QueueByName(context.Context, string, string, string) (Queue, bool, error)
	QueueByURL(context.Context, string) (Queue, bool, error)
	QueueByARN(context.Context, string) (Queue, bool, error)
	ListQueues(context.Context, string, string, string) ([]Queue, error)
	SavePendingAttributes(context.Context, int64, map[string]string, time.Time, time.Time) error
	ListPendingAttributes(context.Context, int64) ([]PendingAttributePatch, error)
	ReplaceTags(context.Context, int64, map[string]string) error
	DeleteTags(context.Context, int64, []string) error
	ListTags(context.Context, int64) (map[string]string, error)
	ListQueuesByRedriveTarget(context.Context, string) ([]Queue, error)
	InsertMoveTask(context.Context, MoveTask) (MoveTask, error)
	UpdateMoveTask(context.Context, MoveTask) error
	ListMoveTasksBySourceQueue(context.Context, int64, int) ([]MoveTask, error)
	FindRunningMoveTask(context.Context, int64) (MoveTask, bool, error)
	ListRunningMoveTasks(context.Context) ([]MoveTask, error)
	InsertMessage(context.Context, Message) (Message, error)
	UpdateMessage(context.Context, Message) error
	ListMessagesByQueue(context.Context, int64) ([]Message, error)
	MessageByRowID(context.Context, int64) (Message, bool, error)
	DeleteMessagesByQueueBefore(context.Context, int64, time.Time) error
	DeleteMessagesByQueue(context.Context, int64) error
	InsertReceipt(context.Context, Receipt) (Receipt, error)
	UpdateReceipt(context.Context, Receipt) error
	ReceiptByHandle(context.Context, string) (Receipt, bool, error)
	DeactivateReceiptsByMessage(context.Context, int64) error
	InsertDedupEntry(context.Context, DedupEntry) (DedupEntry, error)
	DedupEntry(context.Context, int64, string, string, time.Time) (DedupEntry, bool, error)
	DeleteExpiredDedupEntries(context.Context, time.Time) error
	InsertReceiveAttempt(context.Context, ReceiveAttempt) (ReceiveAttempt, error)
	ReceiveAttempt(context.Context, int64, string, time.Time) (ReceiveAttempt, bool, error)
	InvalidateReceiveAttempts(context.Context, int64) error
}
