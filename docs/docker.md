# Docker

## Overview

The repo ships with:

- a multi-stage [Dockerfile](/home/deleema/learning/emulator-aws-sqs/Dockerfile) that builds `sqsd` and runs it in a small non-root runtime image
- a local [compose.yaml](/home/deleema/learning/emulator-aws-sqs/compose.yaml) that publishes the SQS endpoint on the host and persists SQLite state in a Docker volume
- Makefile targets for image build, container run, Compose lifecycle, and host-side AWS CLI smoke testing

## Container design

Build stage:

- uses the official `golang:1.26.1-bookworm` image
- downloads modules
- builds `./cmd/sqsd`
- produces a Linux binary for the Docker target platform with `CGO_ENABLED=0`

Runtime stage:

- uses `gcr.io/distroless/static-debian12:nonroot`
- runs as the distroless non-root user
- writes SQLite data under `/var/lib/sqsd`
- logs to stdout and stderr

This choice keeps the runtime image small while still working with the current pure-Go SQLite dependency.

## Runtime environment variables

Container-friendly server variables:

- `SQS_LISTEN_ADDR`
- `SQS_PUBLIC_BASE_URL`
- `SQS_REGION`
- `SQS_ALLOWED_REGIONS`
- `SQS_ACCOUNT_ID`
- `SQS_SQLITE_PATH`
- `SQS_SQLITE_DSN`
- `SQS_AUTH_MODE`
- `SQS_CREDENTIALS_FILE`
- `SQS_DEFAULT_ACCESS_KEY_ID`
- `SQS_DEFAULT_SECRET_ACCESS_KEY`
- `SQS_DEFAULT_SESSION_TOKEN`
- `SQS_LOG_LEVEL`

Behavior notes:

- `SQS_SQLITE_PATH` is the easiest way to point the container at a persistent SQLite file.
- `SQS_SQLITE_DSN` still exists and overrides `SQS_SQLITE_PATH` when set.
- `SQS_DEFAULT_ACCESS_KEY_ID`, `SQS_DEFAULT_SECRET_ACCESS_KEY`, and `SQS_DEFAULT_SESSION_TOKEN` seed the default strict-mode local credential into the auth registry.

## Persistence

The Docker and Compose workflows mount `/var/lib/sqsd` as a Docker volume.

Default volume:

- `emulator-aws-sqs-data`

SQLite file inside the container:

- `/var/lib/sqsd/sqs.db`

Implications:

- stopping and recreating the container does not remove queue or message state
- removing the Docker volume resets local state completely
- `docker run` and Compose share the same named volume by default
- the Makefile creates that external volume before starting the Compose stack

## Make targets

Build the image:

```bash
make docker-build
```

Run the image directly:

```bash
make docker-run
make docker-logs
make docker-stop
```

Run with Compose:

```bash
make docker-compose-up
make docker-compose-logs
make docker-compose-down
```

Run a host-side AWS CLI smoke test against the compose service:

```bash
make docker-test-cli
```

Remove container artifacts and the persistent Docker volume:

```bash
make docker-clean
```

## Host-side AWS CLI example

```bash
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=us-east-1

aws --endpoint-url http://127.0.0.1:9324 sqs list-queues
```

Or:

```bash
export AWS_ENDPOINT_URL_SQS=http://127.0.0.1:9324
aws sqs list-queues
```

## Development and CI notes

- `make docker-run` is useful when you want one named container and one named Docker volume.
- `make docker-compose-up` is useful when you want a conventional local dev stack with one command.
- CI only verifies that the Docker image builds; it does not run the whole test suite inside Docker.
- The existing Go tests and CLI integration suite still run against the native binary.

## Troubleshooting

- If queue URLs point at the wrong host or port, override `SQS_PUBLIC_BASE_URL`.
- If the host port is already in use, set `DOCKER_HOST_PORT` in the Makefile workflow or `SQS_PORT` in Compose.
- If you want to reset all persisted container state, run `make docker-clean`.
- If you use a custom credential pair, keep the server-side `SQS_DEFAULT_*` values aligned with the host-side AWS CLI or SDK credentials.
