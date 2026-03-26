#!/usr/bin/env bash
set -euo pipefail

ENDPOINT="${SQS_ENDPOINT:-http://127.0.0.1:9324}"

export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}"
export AWS_SESSION_TOKEN="${AWS_SESSION_TOKEN:-}"
export AWS_DEFAULT_REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}"
export AWS_ENDPOINT_URL_SQS="$ENDPOINT"

assert_eq() {
  local got="$1"
  local want="$2"
  local message="$3"
  if [[ "$got" != "$want" ]]; then
    echo "ASSERTION FAILED: $message" >&2
    echo "Expected: $want" >&2
    echo "Actual: $got" >&2
    exit 1
  fi
}

aws_sqs() {
  aws --no-cli-pager sqs "$@"
}

QUEUE_NAME="docker-smoke-$RANDOM-$$"

echo "[docker-cli] create queue"
QUEUE_URL="$(aws_sqs create-queue --queue-name "$QUEUE_NAME" --query QueueUrl --output text)"
sleep 1.1

echo "[docker-cli] list queues"
LIST_OUTPUT="$(aws_sqs list-queues --query 'QueueUrls' --output text)"
if [[ "$LIST_OUTPUT" != *"$QUEUE_URL"* ]]; then
  echo "ASSERTION FAILED: queue URL missing from list-queues output" >&2
  echo "Expected to find: $QUEUE_URL" >&2
  echo "Actual: $LIST_OUTPUT" >&2
  exit 1
fi

echo "[docker-cli] send and receive"
aws_sqs send-message --queue-url "$QUEUE_URL" --message-body "docker-smoke" >/dev/null
BODY="$(aws_sqs receive-message --queue-url "$QUEUE_URL" --max-number-of-messages 1 --query 'Messages[0].Body' --output text)"
assert_eq "$BODY" "docker-smoke" "receive-message should return the sent body"

echo "[docker-cli] success"
