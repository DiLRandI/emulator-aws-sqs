#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
BIN="$TMP_DIR/sqsd"
DB_PATH="$TMP_DIR/sqs.db"
LOG_PATH="$TMP_DIR/sqsd.log"

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]]; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
    wait "$SERVER_PID" >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
ENDPOINT="http://127.0.0.1:${PORT}"

export AWS_ACCESS_KEY_ID="test"
export AWS_SECRET_ACCESS_KEY="test"
export AWS_DEFAULT_REGION="us-east-1"

json_get() {
  local json_input="$1"
  local expr="$2"
  python3 - "$json_input" "$expr" <<'PY'
import json, sys
data = json.loads(sys.argv[1])
expr = sys.argv[2]
cur = data
for part in expr.split('.'):
    if part.isdigit():
        cur = cur[int(part)]
    else:
        cur = cur[part]
if isinstance(cur, (dict, list)):
    print(json.dumps(cur))
else:
    print(cur)
PY
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  local message="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "ASSERTION FAILED: $message" >&2
    echo "Expected to find: $needle" >&2
    echo "Actual: $haystack" >&2
    exit 1
  fi
}

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

assert_empty_response() {
  local value="$1"
  local message="$2"
  if [[ -n "$value" && "$value" != "{}" ]]; then
    echo "ASSERTION FAILED: $message" >&2
    echo "Expected an empty response or {}" >&2
    echo "Actual: $value" >&2
    exit 1
  fi
}

aws_sqs() {
  aws --no-cli-pager --endpoint-url "$ENDPOINT" sqs "$@"
}

echo "[cli] building sqsd"
(cd "$ROOT_DIR" && go build -o "$BIN" ./cmd/sqsd)

echo "[cli] starting sqsd on $ENDPOINT"
(
  cd "$ROOT_DIR"
  SQS_LISTEN_ADDR="127.0.0.1:${PORT}" \
  SQS_PUBLIC_BASE_URL="$ENDPOINT" \
  SQS_SQLITE_DSN="file:${DB_PATH}?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)" \
  SQS_AUTH_MODE="strict" \
  SQS_CREATE_PROPAGATION="0s" \
  SQS_ATTRIBUTE_PROPAGATION="0s" \
  SQS_RETENTION_PROPAGATION="0s" \
  SQS_DELETE_COOLDOWN="1s" \
  SQS_PURGE_COOLDOWN="1s" \
  SQS_LONG_POLL_WAKE_FREQUENCY="50ms" \
  "$BIN" >"$LOG_PATH" 2>&1
) &
SERVER_PID="$!"

for _ in $(seq 1 100); do
  if curl -s -o /dev/null -X POST "$ENDPOINT/" \
      -H 'Content-Type: application/x-amz-json-1.0' \
      -H 'X-Amz-Target: AmazonSQS.ListQueues'; then
    break
  fi
  sleep 0.1
done

echo "[cli] create standard queue"
STD_JSON="$(aws_sqs create-queue --queue-name cli-standard --attributes VisibilityTimeout=1)"
STD_URL="$(json_get "$STD_JSON" "QueueUrl")"

echo "[cli] create fifo queue"
FIFO_JSON="$(aws_sqs create-queue --queue-name cli-fifo.fifo --attributes FifoQueue=true,ContentBasedDeduplication=false,VisibilityTimeout=1)"
FIFO_URL="$(json_get "$FIFO_JSON" "QueueUrl")"

echo "[cli] create content-based fifo queue"
CONTENT_JSON="$(aws_sqs create-queue --queue-name cli-content.fifo --attributes FifoQueue=true,ContentBasedDeduplication=true)"
CONTENT_URL="$(json_get "$CONTENT_JSON" "QueueUrl")"

echo "[cli] send / receive / delete on standard queue"
aws_sqs send-message --queue-url "$STD_URL" --message-body hello >/dev/null
RECV_JSON="$(aws_sqs receive-message --queue-url "$STD_URL" --max-number-of-messages 1 --visibility-timeout 1 --message-system-attribute-names All)"
BODY="$(json_get "$RECV_JSON" "Messages.0.Body")"
HANDLE1="$(json_get "$RECV_JSON" "Messages.0.ReceiptHandle")"
assert_eq "$BODY" "hello" "standard receive body"
COUNT1="$(json_get "$RECV_JSON" "Messages.0.Attributes.ApproximateReceiveCount")"
assert_eq "$COUNT1" "1" "first receive count"

aws_sqs change-message-visibility --queue-url "$STD_URL" --receipt-handle "$HANDLE1" --visibility-timeout 0 >/dev/null
RECV_JSON="$(aws_sqs receive-message --queue-url "$STD_URL" --max-number-of-messages 1 --message-system-attribute-names All)"
HANDLE2="$(json_get "$RECV_JSON" "Messages.0.ReceiptHandle")"
COUNT2="$(json_get "$RECV_JSON" "Messages.0.Attributes.ApproximateReceiveCount")"
assert_eq "$COUNT2" "2" "second receive count"

aws_sqs delete-message --queue-url "$STD_URL" --receipt-handle "$HANDLE2" >/dev/null
EMPTY_JSON="$(aws_sqs receive-message --queue-url "$STD_URL" --max-number-of-messages 1)"
assert_empty_response "$EMPTY_JSON" "queue should be empty after delete"

echo "[cli] send-message-batch and delete-message-batch partial failure"
aws_sqs send-message-batch --queue-url "$STD_URL" \
  --entries \
    Id=a,MessageBody=immediate \
    Id=b,MessageBody=delayed,DelaySeconds=1 >/dev/null
RECV_JSON="$(aws_sqs receive-message --queue-url "$STD_URL" --max-number-of-messages 10 --visibility-timeout 5)"
BODY="$(json_get "$RECV_JSON" "Messages.0.Body")"
HANDLE_BATCH="$(json_get "$RECV_JSON" "Messages.0.ReceiptHandle")"
assert_eq "$BODY" "immediate" "immediate batch message should arrive first"
DEL_BATCH_JSON="$(aws_sqs delete-message-batch --queue-url "$STD_URL" \
  --entries Id=good,ReceiptHandle="$HANDLE_BATCH" Id=bad,ReceiptHandle=bogus)"
assert_eq "$(json_get "$DEL_BATCH_JSON" "Successful.0.Id")" "good" "delete batch success id"
assert_eq "$(json_get "$DEL_BATCH_JSON" "Failed.0.Code")" "ReceiptHandleIsInvalid" "delete batch failure code"

sleep 1.1
RECV_JSON="$(aws_sqs receive-message --queue-url "$STD_URL" --max-number-of-messages 10 --visibility-timeout 5)"
BODY="$(json_get "$RECV_JSON" "Messages.0.Body")"
HANDLE_DELAY="$(json_get "$RECV_JSON" "Messages.0.ReceiptHandle")"
assert_eq "$BODY" "delayed" "delayed batch message should appear after delay"
CHANGE_BATCH_JSON="$(aws_sqs change-message-visibility-batch --queue-url "$STD_URL" \
  --entries Id=good,ReceiptHandle="$HANDLE_DELAY",VisibilityTimeout=0 Id=bad,ReceiptHandle=bogus,VisibilityTimeout=0)"
assert_eq "$(json_get "$CHANGE_BATCH_JSON" "Successful.0.Id")" "good" "change visibility batch success id"
assert_eq "$(json_get "$CHANGE_BATCH_JSON" "Failed.0.Code")" "ReceiptHandleIsInvalid" "change visibility batch failure code"
REAPPEAR_JSON="$(aws_sqs receive-message --queue-url "$STD_URL" --max-number-of-messages 1)"
REAPPEAR_HANDLE="$(json_get "$REAPPEAR_JSON" "Messages.0.ReceiptHandle")"
aws_sqs delete-message --queue-url "$STD_URL" --receipt-handle "$REAPPEAR_HANDLE" >/dev/null

echo "[cli] long polling on empty queue"
START_TS="$(date +%s)"
aws_sqs receive-message --queue-url "$STD_URL" --max-number-of-messages 1 --wait-time-seconds 1 >/dev/null
END_TS="$(date +%s)"
if (( END_TS - START_TS < 1 )); then
  echo "ASSERTION FAILED: expected long poll to wait about one second" >&2
  exit 1
fi

echo "[cli] standard per-message delay"
aws_sqs send-message --queue-url "$STD_URL" --message-body delayed-standard --delay-seconds 1 >/dev/null
NOW_JSON="$(aws_sqs receive-message --queue-url "$STD_URL" --max-number-of-messages 1)"
assert_empty_response "$NOW_JSON" "delayed standard message should not be visible immediately"
sleep 1.1
LATER_JSON="$(aws_sqs receive-message --queue-url "$STD_URL" --max-number-of-messages 1)"
assert_eq "$(json_get "$LATER_JSON" "Messages.0.Body")" "delayed-standard" "delayed standard message should appear"

echo "[cli] FIFO ordering and deduplication"
SEND1="$(aws_sqs send-message --queue-url "$FIFO_URL" --message-body first --message-group-id g1 --message-deduplication-id d1)"
SEND2="$(aws_sqs send-message --queue-url "$FIFO_URL" --message-body second --message-group-id g1 --message-deduplication-id d2)"
SEND_DUP="$(aws_sqs send-message --queue-url "$FIFO_URL" --message-body first --message-group-id g1 --message-deduplication-id d1)"
assert_eq "$(json_get "$SEND1" "MessageId")" "$(json_get "$SEND_DUP" "MessageId")" "fifo duplicate should replay original message id"
FIFO_RECV="$(aws_sqs receive-message --queue-url "$FIFO_URL" --max-number-of-messages 10 --message-system-attribute-names All --receive-request-attempt-id cli-attempt-1)"
assert_eq "$(json_get "$FIFO_RECV" "Messages.0.Body")" "first" "fifo first body"
assert_eq "$(json_get "$FIFO_RECV" "Messages.1.Body")" "second" "fifo second body"
FIFO_REPLAY="$(aws_sqs receive-message --queue-url "$FIFO_URL" --max-number-of-messages 10 --message-system-attribute-names All --receive-request-attempt-id cli-attempt-1)"
assert_eq "$(json_get "$FIFO_RECV" "Messages.0.ReceiptHandle")" "$(json_get "$FIFO_REPLAY" "Messages.0.ReceiptHandle")" "fifo receive attempt replay should preserve receipt handle"

echo "[cli] FIFO per-message delay must fail"
set +e
FIFO_DELAY_ERR="$(aws_sqs send-message --queue-url "$FIFO_URL" --message-body nope --message-group-id g2 --message-deduplication-id d3 --delay-seconds 1 2>&1)"
FIFO_DELAY_STATUS="$?"
set -e
if [[ "$FIFO_DELAY_STATUS" -eq 0 ]]; then
  echo "ASSERTION FAILED: expected FIFO per-message delay to fail" >&2
  exit 1
fi
assert_contains "$FIFO_DELAY_ERR" "InvalidParameterValue" "fifo delay error should mention InvalidParameterValue"

echo "[cli] content-based deduplication"
aws_sqs send-message --queue-url "$CONTENT_URL" --message-body same-body --message-group-id g1 >/dev/null
aws_sqs send-message --queue-url "$CONTENT_URL" --message-body same-body --message-group-id g1 >/dev/null
CONTENT_RECV="$(aws_sqs receive-message --queue-url "$CONTENT_URL" --max-number-of-messages 10)"
assert_eq "$(json_get "$CONTENT_RECV" "Messages.0.Body")" "same-body" "content fifo body"
set +e
json_get "$CONTENT_RECV" "Messages.1.Body" >/dev/null 2>&1
SECOND_STATUS="$?"
set -e
if [[ "$SECOND_STATUS" -eq 0 ]]; then
  echo "ASSERTION FAILED: content-based dedup should not yield a second message" >&2
  exit 1
fi

echo "[cli] DLQ redrive flow"
DLQ_JSON="$(aws_sqs create-queue --queue-name cli-dlq --attributes VisibilityTimeout=1)"
DLQ_URL="$(json_get "$DLQ_JSON" "QueueUrl")"
DLQ_ATTRS="$(aws_sqs get-queue-attributes --queue-url "$DLQ_URL" --attribute-names QueueArn)"
DLQ_ARN="$(json_get "$DLQ_ATTRS" "Attributes.QueueArn")"
REDRIVE_POLICY="$(python3 - <<PY
import json
print(json.dumps({"deadLetterTargetArn": ${DLQ_ARN@Q}, "maxReceiveCount": "1"}))
PY
)"
SRC_ATTRIBUTES="$(python3 - <<PY
import json
print(json.dumps({"VisibilityTimeout": "1", "RedrivePolicy": ${REDRIVE_POLICY@Q}}))
PY
)"
SRC_JSON="$(aws_sqs create-queue --queue-name cli-source --attributes "$SRC_ATTRIBUTES")"
SRC_URL="$(json_get "$SRC_JSON" "QueueUrl")"
SRC_ATTRS="$(aws_sqs get-queue-attributes --queue-url "$SRC_URL" --attribute-names QueueArn)"
SRC_ARN="$(json_get "$SRC_ATTRS" "Attributes.QueueArn")"
aws_sqs send-message --queue-url "$SRC_URL" --message-body to-dlq >/dev/null
aws_sqs receive-message --queue-url "$SRC_URL" --max-number-of-messages 1 --visibility-timeout 1 >/dev/null
sleep 1.1
SOURCE_EMPTY="$(aws_sqs receive-message --queue-url "$SRC_URL" --max-number-of-messages 1)"
assert_empty_response "$SOURCE_EMPTY" "source queue should be empty after DLQ move"
DLQ_RECV="$(aws_sqs receive-message --queue-url "$DLQ_URL" --max-number-of-messages 1 --message-system-attribute-names All)"
assert_eq "$(json_get "$DLQ_RECV" "Messages.0.Body")" "to-dlq" "dlq should receive message"
assert_eq "$(json_get "$DLQ_RECV" "Messages.0.Attributes.DeadLetterQueueSourceArn")" "$SRC_ARN" "dlq source arn should match"
MOVE_JSON="$(aws_sqs start-message-move-task --source-arn "$DLQ_ARN")"
TASK_HANDLE="$(json_get "$MOVE_JSON" "TaskHandle")"
assert_contains "$TASK_HANDLE" "AQEB" "move task handle should look like SQS handle"
for _ in $(seq 1 10); do
  MOVE_LIST="$(aws_sqs list-message-move-tasks --source-arn "$DLQ_ARN" --max-results 10)"
  STATUS="$(json_get "$MOVE_LIST" "Results.0.Status")"
  if [[ "$STATUS" == "COMPLETED" ]]; then
    break
  fi
  sleep 0.1
done
SRC_RECV="$(aws_sqs receive-message --queue-url "$SRC_URL" --max-number-of-messages 1)"
assert_eq "$(json_get "$SRC_RECV" "Messages.0.Body")" "to-dlq" "message should move back to source queue"

echo "[cli] purge sanity"
PURGE_JSON="$(aws_sqs create-queue --queue-name cli-purge)"
PURGE_URL="$(json_get "$PURGE_JSON" "QueueUrl")"
aws_sqs send-message --queue-url "$PURGE_URL" --message-body purge-me >/dev/null
aws_sqs purge-queue --queue-url "$PURGE_URL" >/dev/null
PURGE_RECV="$(aws_sqs receive-message --queue-url "$PURGE_URL" --max-number-of-messages 1)"
assert_empty_response "$PURGE_RECV" "purged queue should be empty"

echo "[cli] AWS_ENDPOINT_URL_SQS support"
AWS_ENDPOINT_URL_SQS="$ENDPOINT" aws --no-cli-pager sqs list-queues >/dev/null

echo "[cli] negative scenario"
set +e
NEG_ERR="$(aws_sqs send-message --queue-url "$ENDPOINT/000000000000/missing" --message-body nope 2>&1)"
NEG_STATUS="$?"
set -e
if [[ "$NEG_STATUS" -eq 0 ]]; then
  echo "ASSERTION FAILED: expected missing queue send to fail" >&2
  exit 1
fi
assert_contains "$NEG_ERR" "QueueDoesNotExist" "negative error should mention QueueDoesNotExist"

echo "[cli] success"
