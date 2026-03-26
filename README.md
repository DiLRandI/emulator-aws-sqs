# emulator-aws-sqs

`emulator-aws-sqs` is a local Amazon SQS-compatible server written in Go. It is intended for local development, integration testing, and self-hosted use where AWS CLI, AWS SDKs, and raw SQS HTTP clients should work against a local endpoint without changing their normal request shape.

## Project Overview

The repo currently implements the full SQS action surface that is wired into this codebase today:

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

Supported protocols:

- AWS JSON protocol
  - `POST /`
  - `X-Amz-Target: AmazonSQS.<Operation>`
  - `Content-Type: application/x-amz-json-1.0`
- AWS Query protocol
  - `Action=<Operation>`
  - `application/x-www-form-urlencoded` request bodies
  - XML responses with SQS namespaces and envelopes

Implementation highlights:

- strict SigV4 request validation by default
- standard and FIFO queues
- SQLite-backed persistence for queues, messages, receipts, dedup windows, receive attempts, tags, and move tasks
- AWS CLI integration coverage
- AWS SDK for Go v2 integration coverage using both `BaseEndpoint` and `EndpointResolverV2`
- raw protocol coverage against the real server process

## Quick Start

### Prerequisites

- Go 1.26+
- Bash
- Python 3
- AWS CLI if you want to use the CLI examples or run the CLI integration suite

### Build

```bash
make build
```

This produces `./bin/sqsd`.

### Run

Foreground:

```bash
make run
```

Background dev workflow:

```bash
make dev-up
make dev-down
```

Discover available workflow targets:

```bash
make help
```

Default runtime values:

- endpoint: `http://127.0.0.1:9324`
- region: `us-east-1`
- account id: `000000000000`
- local test credentials: `test` / `test`
- SQLite DB: `sqs.db` in the current working directory

### Verify The Server Is Up

```bash
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://127.0.0.1:9324 sqs list-queues
```

An empty successful response confirms that `sqsd` is reachable and accepting signed requests.

## Using With AWS CLI

### Endpoint flag workflow

```bash
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://127.0.0.1:9324 sqs create-queue --queue-name demo
aws --endpoint-url http://127.0.0.1:9324 sqs list-queues
```

### `AWS_ENDPOINT_URL_SQS` workflow

```bash
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL_SQS=http://127.0.0.1:9324

aws sqs list-queues
```

### Standard queue create / send / receive / delete

```bash
QUEUE_URL="$(aws --endpoint-url http://127.0.0.1:9324 sqs create-queue --queue-name demo --query QueueUrl --output text)"

aws --endpoint-url http://127.0.0.1:9324 sqs send-message \
  --queue-url "$QUEUE_URL" \
  --message-body "hello from demo"

RECEIVE_JSON="$(aws --endpoint-url http://127.0.0.1:9324 sqs receive-message \
  --queue-url "$QUEUE_URL" \
  --max-number-of-messages 1 \
  --message-system-attribute-names All)"

RECEIPT_HANDLE="$(python3 - <<'PY' "$RECEIVE_JSON"
import json, sys
print(json.loads(sys.argv[1])["Messages"][0]["ReceiptHandle"])
PY
)"

aws --endpoint-url http://127.0.0.1:9324 sqs delete-message \
  --queue-url "$QUEUE_URL" \
  --receipt-handle "$RECEIPT_HANDLE"
```

### FIFO example

```bash
FIFO_URL="$(aws --endpoint-url http://127.0.0.1:9324 sqs create-queue \
  --queue-name demo.fifo \
  --attributes FifoQueue=true,ContentBasedDeduplication=false \
  --query QueueUrl --output text)"

aws --endpoint-url http://127.0.0.1:9324 sqs send-message \
  --queue-url "$FIFO_URL" \
  --message-body "first" \
  --message-group-id "group-1" \
  --message-deduplication-id "dedup-1"

aws --endpoint-url http://127.0.0.1:9324 sqs receive-message \
  --queue-url "$FIFO_URL" \
  --max-number-of-messages 10 \
  --message-system-attribute-names All
```

### DLQ / redrive example

```bash
DLQ_URL="$(aws --endpoint-url http://127.0.0.1:9324 sqs create-queue \
  --queue-name demo-dlq \
  --query QueueUrl --output text)"

DLQ_ARN="$(aws --endpoint-url http://127.0.0.1:9324 sqs get-queue-attributes \
  --queue-url "$DLQ_URL" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' --output text)"

REDRIVE_POLICY="$(python3 - <<'PY' "$DLQ_ARN"
import json, sys
print(json.dumps({"deadLetterTargetArn": sys.argv[1], "maxReceiveCount": "1"}))
PY
)"

SOURCE_URL="$(aws --endpoint-url http://127.0.0.1:9324 sqs create-queue \
  --queue-name demo-source \
  --attributes "$(python3 - <<'PY' "$REDRIVE_POLICY"
import json, sys
print(json.dumps({"VisibilityTimeout": "1", "RedrivePolicy": sys.argv[1]}))
PY
)" \
  --query QueueUrl --output text)"

aws --endpoint-url http://127.0.0.1:9324 sqs send-message \
  --queue-url "$SOURCE_URL" \
  --message-body "to-dlq"
```

For a complete CLI exercise path, run [tests/aws_cli_integration.sh](/home/deleema/learning/emulator-aws-sqs/tests/aws_cli_integration.sh).

## Using With AWS SDK For Go v2

### Minimal `BaseEndpoint` example

```go
package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		panic(err)
	}
	cfg.BaseEndpoint = aws.String("http://127.0.0.1:9324")

	client := sqs.NewFromConfig(cfg)
	out, err := client.ListQueues(context.Background(), &sqs.ListQueuesInput{})
	if err != nil {
		panic(err)
	}
	fmt.Println(out.QueueUrls)
}
```

### Minimal `EndpointResolverV2` example

```go
package main

import (
	"context"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	smithyendpoints "github.com/aws/smithy-go/endpoints"
)

type localResolver struct{}

func (localResolver) ResolveEndpoint(ctx context.Context, params sqs.EndpointParameters) (smithyendpoints.Endpoint, error) {
	uri, err := url.Parse("http://127.0.0.1:9324")
	if err != nil {
		return smithyendpoints.Endpoint{}, err
	}
	return smithyendpoints.Endpoint{URI: *uri}, nil
}

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		panic(err)
	}

	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		o.EndpointResolverV2 = localResolver{}
	})

	_, _ = client.ListQueues(context.Background(), &sqs.ListQueuesInput{})
}
```

Local credentials and region for SDK use:

- access key: `test`
- secret key: `test`
- session token: not required
- region: `us-east-1`

## Running Tests

### All Go tests

```bash
make test
```

### Lightweight in-process Go tests

```bash
make test-unit
```

### Go integration tests against the real `sqsd` process

```bash
make test-integration
```

### AWS CLI integration suite

```bash
make test-cli
```

By default, `make test-cli` runs the CLI suite against an isolated temporary SQLite database on `http://127.0.0.1:19324`, so it does not collide with a dev server started by `make dev-up`.

### Everything

```bash
make test-all
make ci
```

CLI test prerequisites:

- AWS CLI installed and on `PATH`
- Bash
- Python 3

## Repo Layout

- [cmd/sqsd](/home/deleema/learning/emulator-aws-sqs/cmd/sqsd): server entrypoint
- [internal/auth](/home/deleema/learning/emulator-aws-sqs/internal/auth): SigV4 validation and local credential registry
- [internal/model](/home/deleema/learning/emulator-aws-sqs/internal/model): generated SQS operation and shape registry
- [internal/protocol/json](/home/deleema/learning/emulator-aws-sqs/internal/protocol/json): AWS JSON protocol codec
- [internal/protocol/query](/home/deleema/learning/emulator-aws-sqs/internal/protocol/query): AWS Query protocol decode/encode layer
- [internal/service](/home/deleema/learning/emulator-aws-sqs/internal/service): queue and message semantics
- [internal/storage/sqlite](/home/deleema/learning/emulator-aws-sqs/internal/storage/sqlite): SQLite persistence layer
- [internal/tests](/home/deleema/learning/emulator-aws-sqs/internal/tests): raw protocol, lightweight handler, and SDK integration tests
- [tests/aws_cli_integration.sh](/home/deleema/learning/emulator-aws-sqs/tests/aws_cli_integration.sh): shell-based AWS CLI suite
- [tools/gensqs](/home/deleema/learning/emulator-aws-sqs/tools/gensqs): botocore-to-registry generator

## Storage And Runtime Behavior

- SQLite is the durable backing store.
- By default, the server uses `sqs.db` in the current working directory.
- WAL mode and foreign keys are enabled in the default DSN.
- Queue URLs default to `http://127.0.0.1:9324/<account-id>/<queue-name>`.

Reset local state:

```bash
make clean
rm -f sqs.db sqs.db-shm sqs.db-wal
```

Or point the server at a different DB:

```bash
SQS_SQLITE_DSN='file:my-local.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)' make run
```

The AWS CLI integration suite does not reuse `sqs.db` by default. It uses a temporary database unless you override `TEST_SQS_DB_PATH`.

## Compatibility Notes

This repo aims to be SQS-compatible, but it is not yet a bit-for-bit AWS clone in every externally observable corner.

Known AWS-parity gaps in the current codebase:

- Standard queues are deterministic. The current implementation delivers standard-queue messages in a stable order and does not intentionally simulate AWS-style duplicate delivery, out-of-order delivery, or weighted short-poll host sampling.
- Queue policies are stored but not enforced on data-plane access. `AddPermission`, `RemovePermission`, and the `Policy` queue attribute mutate stored policy state, but send/receive/delete/change-visibility authorization is not evaluated against IAM-style resource policies.
- `RedriveAllowPolicy` is stored as a queue attribute but is not enforced during DLQ configuration or redrive.
- SSE/KMS queue attributes are accepted and surfaced as queue attributes, but there is no real message encryption, KMS integration, key policy handling, or KMS-specific runtime behavior behind them.
- Approximate queue counters are more exact than AWS. `ApproximateNumberOfMessages*` values are derived directly from persisted rows instead of following AWS’s intentionally approximate and laggy counter semantics.
- Request throttling, in-flight caps, and other AWS production quotas are not fully modeled.
- Message move task progression is request-driven. Tasks advance when API calls trigger service maintenance rather than via an autonomous scheduler loop.

Those gaps are also tracked in [docs/compatibility.md](/home/deleema/learning/emulator-aws-sqs/docs/compatibility.md).

## Roadmap / Next Gaps

Real next parity work:

- introduce deliberate standard-queue duplicate/out-of-order behavior modeling
- implement IAM-style resource-policy enforcement on queue operations
- implement real SSE-SQS/SSE-KMS visible behavior instead of attribute-only surfacing
- move message move tasks from request-driven advancement to a background scheduler
- model AWS-like throttling and more realistic approximate counters

## Contributing / Development Notes

Common commands:

```bash
make help
make build
make dev-up
make test-integration
make test-cli
make ci
```

Useful overrides:

```bash
AWS_REGION=us-west-2 SQS_PORT=9444 SQS_DB_PATH=local-dev.db make dev-up
TEST_SQS_PORT=19444 TEST_SQS_DB_PATH=.tmp/cli-tests.db make test-cli
```

Focused test examples:

```bash
go test ./internal/tests -run TestRawJSONLifecycleSigned -v
go test ./internal/tests -run TestSDKFIFOAndDLQResolverV2 -v
```

Registry regeneration:

```bash
go run ./tools/gensqs \
  -service-model /path/to/service-2.json.gz \
  -paginators /path/to/paginators-1.json \
  -out ./internal/model/registry_gen.go
```

Additional developer docs:

- [docs/development.md](/home/deleema/learning/emulator-aws-sqs/docs/development.md)
- [docs/testing.md](/home/deleema/learning/emulator-aws-sqs/docs/testing.md)
- [docs/compatibility.md](/home/deleema/learning/emulator-aws-sqs/docs/compatibility.md)
