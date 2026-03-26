.DEFAULT_GOAL := help

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

GO ?= go
BASH ?= bash

BIN_DIR ?= bin
BINARY ?= $(BIN_DIR)/sqsd
DEV_DIR ?= .dev
DEV_PID_FILE ?= $(DEV_DIR)/sqsd.pid
DEV_LOG_FILE ?= $(DEV_DIR)/sqsd.log

AWS_ACCESS_KEY_ID ?= test
AWS_SECRET_ACCESS_KEY ?= test
AWS_SESSION_TOKEN ?=
AWS_REGION ?= us-east-1

SQS_HOST ?= 127.0.0.1
SQS_PORT ?= 9324
SQS_ENDPOINT ?= http://$(SQS_HOST):$(SQS_PORT)
SQS_DB_PATH ?= sqs.db
SQS_SQLITE_DSN ?= file:$(SQS_DB_PATH)?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)

TEST_SQS_HOST ?= 127.0.0.1
TEST_SQS_PORT ?= 19324
TEST_SQS_ENDPOINT ?= http://$(TEST_SQS_HOST):$(TEST_SQS_PORT)
TEST_SQS_DB_PATH ?=

UNIT_TEST_RUN ?= Test(CreateQueueJSONAndListQuery|SetAndGetQueueAttributesJSON)
INTEGRATION_TEST_RUN ?= Test(Raw|SDK)

GOFMT_DIRS := cmd internal tests tools

.PHONY: help build run test test-unit test-integration test-cli test-all fmt lint tidy clean ci dev-up dev-down fmt-check tidy-check

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "; print "Targets:"} /^[a-zA-Z0-9_.-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build ./bin/sqsd
	@mkdir -p "$(BIN_DIR)"
	$(GO) build -o "$(BINARY)" ./cmd/sqsd

run: ## Run sqsd locally in the foreground
	SQS_LISTEN_ADDR="$(SQS_HOST):$(SQS_PORT)" \
	SQS_PUBLIC_BASE_URL="$(SQS_ENDPOINT)" \
	SQS_REGION="$(AWS_REGION)" \
	SQS_ALLOWED_REGIONS="$(AWS_REGION)" \
	SQS_SQLITE_DSN="$(SQS_SQLITE_DSN)" \
	$(GO) run ./cmd/sqsd

test: ## Run all Go tests
	$(GO) test ./...

test-unit: ## Run lightweight in-process Go tests
	$(GO) test ./... -run '$(UNIT_TEST_RUN)'

test-integration: ## Run Go integration tests that spawn the real sqsd process
	$(GO) test ./internal/tests -run '$(INTEGRATION_TEST_RUN)'

test-cli: ## Run the AWS CLI integration suite
	AWS_ACCESS_KEY_ID="$(AWS_ACCESS_KEY_ID)" \
	AWS_SECRET_ACCESS_KEY="$(AWS_SECRET_ACCESS_KEY)" \
	AWS_SESSION_TOKEN="$(AWS_SESSION_TOKEN)" \
	AWS_REGION="$(AWS_REGION)" \
	SQS_HOST="$(TEST_SQS_HOST)" \
	SQS_PORT="$(TEST_SQS_PORT)" \
	SQS_ENDPOINT="$(TEST_SQS_ENDPOINT)" \
	SQS_DB_PATH="$(TEST_SQS_DB_PATH)" \
	./tests/aws_cli_integration.sh

test-all: ## Run Go tests and AWS CLI integration tests
	$(MAKE) test
	$(MAKE) test-cli

fmt: ## Format Go sources with gofmt
	@find $(GOFMT_DIRS) -type f -name '*.go' -print0 | xargs -0 gofmt -w

lint: ## Run minimal lint checks: go vet and shell syntax validation
	$(GO) vet ./...
	$(BASH) -n ./tests/aws_cli_integration.sh

tidy: ## Run go mod tidy
	$(GO) mod tidy

clean: ## Remove built artifacts, local dev files, and default SQLite state
	-@$(MAKE) dev-down >/dev/null 2>&1 || true
	rm -rf "$(BIN_DIR)" "$(DEV_DIR)"
	rm -f sqs.db sqs.db-shm sqs.db-wal

ci: ## Run formatting checks, lint, Go tests, and CLI tests
	$(MAKE) fmt-check
	$(MAKE) tidy-check
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) test-cli

dev-up: build ## Start sqsd in the background for local development
	@mkdir -p "$(DEV_DIR)"
	@if [[ -f "$(DEV_PID_FILE)" ]] && kill -0 "$$(cat "$(DEV_PID_FILE)")" 2>/dev/null; then \
		echo "sqsd already running with pid $$(cat "$(DEV_PID_FILE)")"; \
		exit 0; \
	fi
	@rm -f "$(DEV_PID_FILE)"
	@launcher="$$(command -v setsid || true)"; \
	if [[ -n "$$launcher" ]]; then \
		"$$launcher" env \
			SQS_LISTEN_ADDR="$(SQS_HOST):$(SQS_PORT)" \
			SQS_PUBLIC_BASE_URL="$(SQS_ENDPOINT)" \
			SQS_REGION="$(AWS_REGION)" \
			SQS_ALLOWED_REGIONS="$(AWS_REGION)" \
			SQS_SQLITE_DSN="$(SQS_SQLITE_DSN)" \
			"$(abspath $(BINARY))" >"$(DEV_LOG_FILE)" 2>&1 < /dev/null & \
			pid="$$!"; \
	else \
		env \
			SQS_LISTEN_ADDR="$(SQS_HOST):$(SQS_PORT)" \
			SQS_PUBLIC_BASE_URL="$(SQS_ENDPOINT)" \
			SQS_REGION="$(AWS_REGION)" \
			SQS_ALLOWED_REGIONS="$(AWS_REGION)" \
			SQS_SQLITE_DSN="$(SQS_SQLITE_DSN)" \
			"$(abspath $(BINARY))" >"$(DEV_LOG_FILE)" 2>&1 < /dev/null & \
			pid="$$!"; \
			disown "$$pid"; \
	fi; \
		echo "$$pid" >"$(DEV_PID_FILE)"; \
		sleep 1; \
		if ! kill -0 "$$pid" 2>/dev/null; then \
			echo "sqsd failed to start; see $(DEV_LOG_FILE)"; \
			rm -f "$(DEV_PID_FILE)"; \
			exit 1; \
		fi
	@echo "sqsd started on $(SQS_ENDPOINT) with pid $$(cat "$(DEV_PID_FILE)")"
	@echo "logs: $(DEV_LOG_FILE)"

dev-down: ## Stop the background dev server started by make dev-up
	@if [[ -f "$(DEV_PID_FILE)" ]] && kill -0 "$$(cat "$(DEV_PID_FILE)")" 2>/dev/null; then \
		kill "$$(cat "$(DEV_PID_FILE)")"; \
		wait "$$(cat "$(DEV_PID_FILE)")" 2>/dev/null || true; \
		rm -f "$(DEV_PID_FILE)"; \
		echo "sqsd stopped"; \
	else \
		rm -f "$(DEV_PID_FILE)"; \
		echo "sqsd is not running"; \
	fi

fmt-check: ## Check that Go sources are formatted
	@unformatted="$$(find $(GOFMT_DIRS) -type f -name '*.go' -print0 | xargs -0 gofmt -l)"; \
	if [[ -n "$$unformatted" ]]; then \
		echo "The following files need gofmt:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

tidy-check: ## Check that go.mod and go.sum are already tidy
	@tmpdir="$$(mktemp -d)"; \
	cp go.mod "$$tmpdir/go.mod"; \
	cp go.sum "$$tmpdir/go.sum"; \
	$(GO) mod tidy >/dev/null 2>&1; \
	status=0; \
	if ! cmp -s go.mod "$$tmpdir/go.mod" || ! cmp -s go.sum "$$tmpdir/go.sum"; then \
		echo "go.mod/go.sum are not tidy. Run 'make tidy'."; \
		status=1; \
	fi; \
	mv "$$tmpdir/go.mod" go.mod; \
	mv "$$tmpdir/go.sum" go.sum; \
	rmdir "$$tmpdir"; \
	exit $$status
