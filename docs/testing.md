# Testing

The repo has three main test layers plus a small in-process handler test slice.

## Test suites

### 1. Lightweight in-process handler tests

File:

- [internal/tests/server_test.go](/home/deleema/learning/emulator-aws-sqs/internal/tests/server_test.go)

What they do:

- exercise the HTTP handler with an in-memory SQLite database
- validate a few fast control-plane paths without spawning the real binary

Run:

```bash
make test-unit
```

### 2. Raw protocol conformance tests

File:

- [internal/tests/raw_protocol_test.go](/home/deleema/learning/emulator-aws-sqs/internal/tests/raw_protocol_test.go)

What they do:

- build and spawn the real `sqsd` binary
- send signed AWS JSON protocol requests
- send signed AWS Query protocol requests
- verify content types, status codes, JSON envelopes, XML namespaces, batch behavior, long polling, and error surfaces

Run:

```bash
go test ./internal/tests -run 'TestRaw' -v
make test-integration
```

### 3. AWS SDK for Go v2 integration tests

File:

- [internal/tests/sdk_integration_test.go](/home/deleema/learning/emulator-aws-sqs/internal/tests/sdk_integration_test.go)

What they do:

- build and spawn the real `sqsd` binary
- use the official AWS SDK for Go v2
- cover both `BaseEndpoint` and `EndpointResolverV2`
- exercise standard queues, FIFO queues, visibility handling, batch APIs, DLQ flow, and move tasks

Run:

```bash
go test ./internal/tests -run 'TestSDK' -v
make test-integration
```

### 4. AWS CLI integration suite

File:

- [tests/aws_cli_integration.sh](/home/deleema/learning/emulator-aws-sqs/tests/aws_cli_integration.sh)

What it does:

- builds `sqsd`
- boots it in strict SigV4 mode
- uses the real AWS CLI against the local endpoint
- validates queue lifecycle, message lifecycle, FIFO behavior, DLQ flow, purge, and endpoint env support

Run:

```bash
make test-cli
./tests/aws_cli_integration.sh
```

Default behavior:

- `make test-cli` runs on `http://127.0.0.1:19324`
- the script uses a temporary SQLite database unless `TEST_SQS_DB_PATH` or `SQS_DB_PATH` is set
- this keeps the CLI suite isolated from `make dev-up`

### 5. Docker AWS CLI smoke test

File:

- [tests/aws_cli_docker_smoke.sh](/home/deleema/learning/emulator-aws-sqs/tests/aws_cli_docker_smoke.sh)

What it does:

- starts the Compose-managed container through `make docker-test-cli`
- uses the host AWS CLI against the containerized endpoint
- verifies container reachability, signing, queue creation, listing, and a send/receive flow
- uses the same named Docker volume that `make docker-run` uses, so the container workflows stay consistent

Run:

```bash
make docker-test-cli
```

## Recommended commands

Run all Go tests:

```bash
make test
```

Run integration-heavy Go tests only:

```bash
make test-integration
```

Run everything including the AWS CLI suite:

```bash
make test-all
make ci
```

Run the Docker-specific smoke path:

```bash
make docker-build
make docker-test-cli
```

## Interpreting failures

### Raw protocol failures

Usually indicate:

- broken JSON or Query decoding
- incorrect XML envelope or member naming
- missing SigV4 compatibility
- wrong content type or error shape

Check:

- [internal/protocol/json/codec.go](/home/deleema/learning/emulator-aws-sqs/internal/protocol/json/codec.go)
- [internal/protocol/query/decode.go](/home/deleema/learning/emulator-aws-sqs/internal/protocol/query/decode.go)
- [internal/protocol/query/encode.go](/home/deleema/learning/emulator-aws-sqs/internal/protocol/query/encode.go)
- [internal/protocol/query/names.go](/home/deleema/learning/emulator-aws-sqs/internal/protocol/query/names.go)

### SDK integration failures

Usually indicate:

- wire compatibility drift
- request/response shape mismatch
- auth/signing mismatch
- receipt handle, FIFO, or redrive semantics drift

Check:

- [internal/service/messages.go](/home/deleema/learning/emulator-aws-sqs/internal/service/messages.go)
- [internal/service/sqs.go](/home/deleema/learning/emulator-aws-sqs/internal/service/sqs.go)
- [internal/auth/auth.go](/home/deleema/learning/emulator-aws-sqs/internal/auth/auth.go)

### CLI integration failures

Usually indicate:

- response-shape or field-name differences the CLI notices
- incorrect error codes or malformed XML/JSON payloads
- incompatibility with CLI argument expectations

The script prints the stage it is running. Failures are intended to be loud and localize quickly.

## Test prerequisites

- Go 1.26+
- Bash
- Python 3
- AWS CLI for `make test-cli` and `make ci`
- Docker and Docker Compose for `make docker-build`, `make docker-compose-up`, and `make docker-test-cli`

## CI path

The GitHub Actions workflow runs:

```bash
make ci
make docker-build
```

That includes:

- formatting check
- tidy check
- `go vet`
- all Go tests
- AWS CLI integration tests
- Docker image build verification
