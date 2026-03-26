# syntax=docker/dockerfile:1.7

FROM golang:1.26.1-bookworm AS builder

WORKDIR /src

ARG TARGETOS=linux
ARG TARGETARCH=amd64

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/sqsd ./cmd/sqsd && \
    mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/sqsd /sqsd
COPY --chown=nonroot:nonroot --from=builder /out/data /var/lib/sqsd

ENV SQS_LISTEN_ADDR=0.0.0.0:9324 \
    SQS_PUBLIC_BASE_URL=http://127.0.0.1:9324 \
    SQS_REGION=us-east-1 \
    SQS_ALLOWED_REGIONS=us-east-1 \
    SQS_ACCOUNT_ID=000000000000 \
    SQS_SQLITE_PATH=/var/lib/sqsd/sqs.db \
    SQS_AUTH_MODE=strict \
    SQS_DEFAULT_ACCESS_KEY_ID=test \
    SQS_DEFAULT_SECRET_ACCESS_KEY=test \
    SQS_DEFAULT_SESSION_TOKEN= \
    SQS_LOG_LEVEL=INFO

WORKDIR /var/lib/sqsd
VOLUME ["/var/lib/sqsd"]
EXPOSE 9324

ENTRYPOINT ["/sqsd"]
