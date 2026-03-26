# Compatibility

## Implemented actions

The current codebase implements these SQS actions:

- `AddPermission`
- `CancelMessageMoveTask`
- `ChangeMessageVisibility`
- `ChangeMessageVisibilityBatch`
- `CreateQueue`
- `DeleteMessage`
- `DeleteMessageBatch`
- `DeleteQueue`
- `GetQueueAttributes`
- `GetQueueUrl`
- `ListDeadLetterSourceQueues`
- `ListMessageMoveTasks`
- `ListQueueTags`
- `ListQueues`
- `PurgeQueue`
- `ReceiveMessage`
- `RemovePermission`
- `SendMessage`
- `SendMessageBatch`
- `SetQueueAttributes`
- `StartMessageMoveTask`
- `TagQueue`
- `UntagQueue`

## Supported protocols

- AWS JSON protocol
- AWS Query protocol

Both protocols map into the same internal action handlers.

## Current strengths

- strict SigV4 signing support for normal AWS CLI and SDK usage
- AWS CLI integration coverage against the real server
- AWS SDK for Go v2 integration coverage using both endpoint configuration styles
- raw signed HTTP conformance coverage
- standard queue and FIFO queue support
- persisted messages, receipts, dedup ledger, receive-attempt cache, and move-task records

## Known parity gaps

These are real, current differences from AWS SQS behavior that are visible from the code and tests.

### Standard queue behavior is more deterministic than AWS

The implementation returns standard-queue messages in a stable persisted order. It does not intentionally model:

- duplicate delivery under normal operation
- out-of-order delivery
- AWS short-poll host sampling behavior

That means local tests can be more stable than production behavior.

### Queue policies are stored, not enforced

`AddPermission`, `RemovePermission`, and direct `Policy` attribute updates persist policy JSON on the queue, but there is no IAM-style authorization evaluation for send/receive/delete/change-visibility operations.

Practical effect:

- local credentials are authenticated
- queue policies are not used to allow or deny data-plane access

### `RedriveAllowPolicy` is not enforced

The attribute exists and can be set or read, but the current redrive/DLQ logic does not enforce it.

### SSE/KMS is attribute-visible only

Queue attributes such as:

- `KmsMasterKeyId`
- `KmsDataKeyReusePeriodSeconds`
- `SqsManagedSseEnabled`

are accepted and surfaced, but the runtime does not currently:

- encrypt message payloads
- call KMS
- enforce KMS key usage or key policy behavior
- reproduce KMS-specific runtime semantics

### Approximate counters are effectively exact counters

`ApproximateNumberOfMessages`, `ApproximateNumberOfMessagesNotVisible`, and `ApproximateNumberOfMessagesDelayed` are derived from current persisted state instead of following AWS’s intentionally approximate, eventually consistent counter behavior.

### AWS throttling and quota semantics are incomplete

The repo does not currently model production-style:

- request throttling
- account-level throughput limits
- in-flight caps
- other service quotas that AWS exposes under load

### Message move tasks are request-driven

Move tasks progress when API calls trigger service maintenance. They are not currently advanced by a dedicated background scheduler loop.

## Future parity work

Real next improvements for compatibility:

- model standard-queue duplicates and out-of-order delivery more realistically
- implement IAM-style queue policy enforcement
- implement real SSE-SQS and SSE-KMS visible behavior
- add autonomous move-task scheduling
- model throttling and more AWS-like approximate counters
