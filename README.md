# emulator-aws-sqs

Local Amazon SQS-compatible server in Go with strict SigV4 auth, AWS JSON and Query front doors, durable SQLite backing storage, and end-to-end compatibility coverage for AWS CLI and AWS SDK for Go v2.

## Supported actions

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

## Features in the current implementation

- AWS JSON protocol:
  - `POST /`
  - `X-Amz-Target: AmazonSQS.<Operation>`
  - `Content-Type: application/x-amz-json-1.0`
- AWS Query protocol:
  - `Action=<Operation>`
  - form-encoded request bodies
  - XML response envelopes and namespaces
  - queue-path requests such as `/<account-id>/<queue-name>`
- Strict SigV4 validation by default for local custom endpoints
- Durable SQLite storage for queues, messages, receipts, dedup windows, receive attempts, tags, and move tasks
- Standard queues
- FIFO queues with group ordering and 5-minute dedup windows
- Delay queues and per-message delay on standard queues
- Long polling
- Visibility timeout leasing and receipt handle rotation
- DLQ redrive on receive count and message move tasks operating on persisted messages

## Run

```bash
go run ./cmd/sqsd
```

Default local settings:

- listen address: `:9324`
- public base URL: `http://127.0.0.1:9324`
- region: `us-east-1`
- default local credentials: access key `test`, secret `test`
- default account id: `000000000000`
- SQLite DSN: `file:sqs.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)`
- auth mode: `strict`

Useful env vars:

- `SQS_LISTEN_ADDR`
- `SQS_PUBLIC_BASE_URL`
- `SQS_REGION`
- `SQS_ALLOWED_REGIONS`
- `SQS_ACCOUNT_ID`
- `SQS_SQLITE_DSN`
- `SQS_AUTH_MODE`
- `SQS_CREDENTIALS_FILE`
- `SQS_LOG_LEVEL`
- `SQS_CREATE_PROPAGATION`
- `SQS_DELETE_COOLDOWN`
- `SQS_PURGE_COOLDOWN`
- `SQS_ATTRIBUTE_PROPAGATION`
- `SQS_RETENTION_PROPAGATION`
- `SQS_LONG_POLL_WAKE_FREQUENCY`

## AWS CLI usage

```bash
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://127.0.0.1:9324 sqs create-queue --queue-name demo
aws --endpoint-url http://127.0.0.1:9324 sqs list-queues
aws --endpoint-url http://127.0.0.1:9324 sqs send-message \
  --queue-url http://127.0.0.1:9324/000000000000/demo \
  --message-body hello
```

Service-specific endpoint env is also supported:

```bash
export AWS_ENDPOINT_URL_SQS=http://127.0.0.1:9324
aws sqs list-queues
```

## Tests

Run the Go conformance and SDK integration suites:

```bash
go test ./...
```

Run the AWS CLI integration suite:

```bash
tests/aws_cli_integration.sh
```

The Go suite includes:

- raw signed HTTP tests against the real `sqsd` process
- JSON protocol conformance checks
- Query protocol XML envelope and namespace checks
- AWS SDK for Go v2 integration tests using both `BaseEndpoint` and `EndpointResolverV2`

The CLI suite boots the real server in strict-signing mode and exercises:

- standard queue send, receive, delete, batch, change visibility, delay, and long polling
- FIFO ordering and deduplication
- content-based deduplication
- DLQ and message move task flows
- purge sanity
- `AWS_ENDPOINT_URL_SQS`

## Layout

- `cmd/sqsd`: server entrypoint
- `internal/auth`: SigV4 validation and local credential registry
- `internal/model`: generated SQS operation and shape registry
- `internal/protocol/json`: AWS JSON protocol codec
- `internal/protocol/query`: AWS Query protocol codec and XML serializer
- `internal/service`: SQS action handlers and queue/message semantics
- `internal/storage/sqlite`: durable SQLite backing store
- `internal/tests`: raw protocol and AWS SDK integration tests
- `tests/aws_cli_integration.sh`: shell-based AWS CLI integration suite
