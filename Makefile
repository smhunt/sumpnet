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

.PHONY: all tools proto lint fmt test test-integration sim build up down logs ps env clean migrate-up migrate-down migrate-new sqlc db-shell

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

## test-integration: tests behind the integration build tag (need Docker; testcontainers)
test-integration:
	go test -race -count=1 -tags integration ./...

## sim: replay a scenario into the compose stack's Mosquitto (SCENARIO, SEED, SPEED, HOMES)
SCENARIO ?= storm50
SEED     ?= 42
SPEED    ?= 60
HOMES    ?= 60
sim: env
	@mkdir -p loadtest/results
	go run ./cmd/simulator -scenario $(SCENARIO) -seed $(SEED) -speed $(SPEED) -homes $(HOMES) \
	  -sink mqtt -mqtt-url mqtt://localhost:$$(grep '^MQTT_PORT=' deploy/compose/.env | cut -d= -f2) \
	  -truth-out loadtest/results/sim-truth-$(SCENARIO)-$(SEED).json -hash

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

## Database (values from deploy/compose/.env)
DB_URL ?= postgres://sumpnet:$$(grep '^POSTGRES_PASSWORD=' deploy/compose/.env | cut -d= -f2)@localhost:$$(grep '^POSTGRES_PORT=' deploy/compose/.env | cut -d= -f2)/sumpnet?sslmode=disable

migrate-up: $(MIGRATE) env
	$(MIGRATE) -path migrations -database "$(DB_URL)" up

migrate-down: $(MIGRATE) env
	$(MIGRATE) -path migrations -database "$(DB_URL)" down 1

## migrate-new: create migrations/NNNN_$(NAME).{up,down}.sql
migrate-new: $(MIGRATE)
	$(MIGRATE) create -ext sql -dir migrations -seq $(NAME)

## sqlc: regenerate internal/store/sqlcgen
sqlc: $(SQLC)
	$(SQLC) generate

db-shell: env
	$(COMPOSE) exec postgres psql -U sumpnet -d sumpnet

## env: create deploy/compose/.env from the example if missing
env:
	@test -f deploy/compose/.env || cp deploy/compose/.env.example deploy/compose/.env

clean:
	rm -rf $(BIN) gen
