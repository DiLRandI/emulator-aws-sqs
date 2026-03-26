# emulator-aws-sqs

Local Amazon SQS-compatible server in Go.

## Current status

This repository now contains the production-oriented foundation for the emulator:

- official SQS model registry generated from botocore
- AWS JSON and AWS Query front doors mapped to one dispatcher
- strict SigV4 validation for local custom endpoints
- durable SQLite storage
- runnable `sqsd` server entrypoint
- implemented control-plane actions:
  - `AddPermission`
  - `CancelMessageMoveTask`
  - `CreateQueue`
  - `DeleteQueue`
  - `GetQueueAttributes`
  - `GetQueueUrl`
  - `ListDeadLetterSourceQueues`
  - `ListMessageMoveTasks`
  - `ListQueueTags`
  - `ListQueues`
  - `PurgeQueue`
  - `RemovePermission`
  - `SetQueueAttributes`
  - `StartMessageMoveTask`
  - `TagQueue`
  - `UntagQueue`

The message-plane actions are the next tranche.

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

## Test

```bash
go test ./...
```

## Layout

- `cmd/sqsd`: server entrypoint
- `internal/auth`: SigV4 validation and local credential registry
- `internal/model`: generated SQS operation and shape registry
- `internal/protocol/json`: AWS JSON protocol codec
- `internal/protocol/query`: AWS Query protocol codec and XML serializer
- `internal/service`: SQS action handlers
- `internal/storage/sqlite`: durable SQLite backing store
- `internal/tests`: protocol-level tests
