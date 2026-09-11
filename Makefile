include .versions.env
export

BIN      := $(CURDIR)/bin
BUF      := $(BIN)/buf
GOLANGCI := $(BIN)/golangci-lint
SQLC     := $(BIN)/sqlc
MIGRATE  := $(BIN)/migrate
COMPOSE  := docker compose -f deploy/compose/docker-compose.yml
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

.PHONY: all tools proto lint fmt test build up down logs ps env clean

all: lint test

## tools: install pinned dev tools (see .versions.env) into ./bin
tools: $(BUF) $(GOLANGCI) $(SQLC) $(MIGRATE)

$(BUF):
	GOBIN=$(BIN) go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

$(SQLC):
	GOBIN=$(BIN) go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)

$(MIGRATE):
	GOBIN=$(BIN) go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)

# golangci-lint disclaims `go install` builds; use the release binary.
$(GOLANGCI):
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b $(BIN) $(GOLANGCI_LINT_VERSION)

## proto: regenerate gen/go from proto/
proto: $(BUF)
	$(BUF) generate

## lint: buf lint + golangci-lint (linters and formatters)
lint: $(BUF) $(GOLANGCI)
	$(BUF) lint
	$(BUF) format --diff --exit-code
	$(GOLANGCI) run ./...
	$(GOLANGCI) fmt --diff ./...

fmt: $(BUF) $(GOLANGCI)
	$(BUF) format -w
	$(GOLANGCI) fmt ./...

test:
	go test -race -count=1 -cover ./...

## build: build every service image via compose
build: env
	VERSION=$(VERSION) COMMIT=$(COMMIT) $(COMPOSE) build

## up: bring the full stack up and wait for every healthcheck
up: env
	VERSION=$(VERSION) COMMIT=$(COMMIT) $(COMPOSE) up -d --build --wait

down:
	$(COMPOSE) down

## logs: follow logs (S=service to filter)
logs:
	$(COMPOSE) logs -f $(S)

ps:
	$(COMPOSE) ps

## env: create deploy/compose/.env from the example if missing
env:
	@test -f deploy/compose/.env || cp deploy/compose/.env.example deploy/compose/.env

clean:
	rm -rf $(BIN) gen
