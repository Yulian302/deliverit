# DeliverIt — developer entry points.
# Keep in sync with the golangci-lint-action `version:` in .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION := v2.15.0

# Per-service local config (git-ignored copies of the committed .env.example).
GATEWAY_ENV := services/gateway/.env
ORDER_ENV   := services/order/.env

# Throwaway local Postgres for running Order outside compose (`make db-up`).
# Its credentials match DATABASE_URL in services/order/.env.example.
DB_CONTAINER := deliverit-pg
DB_IMAGE     := postgres:17-alpine

# load-env FILE: export every KEY=value in FILE into this recipe's shell.
# Plain shell sourcing: no extra tools, and the binary never reads files.
load-env = set -a && . ./$(1) && set +a

# Dev tools (buf, protoc plugins, goose) are pinned in their own module,
# tools/go.mod, so their dependencies never change the services' versions.
TOOL := go tool -modfile=tools/go.mod

# sqlc runs from its official image (pinned): it isn't `go tool`-friendly.
# -u keeps generated files owned by you, not root.
SQLC_IMAGE := sqlc/sqlc:1.31.1
SQLC = docker run --rm -u $(shell id -u):$(shell id -g) -v $(CURDIR):/src -w /src/$(1) $(SQLC_IMAGE)

# The full stack in Docker (deploy/compose/docker-compose.yml).
COMPOSE := docker compose -f deploy/compose/docker-compose.yml

.PHONY: test test-int lint gen gen-check run-gateway run-order migrate migration db-up db-down \
	up down clean logs ps smoke

## up: build images and start the whole stack; returns once everything is healthy
up:
	$(COMPOSE) up -d --build --wait --wait-timeout 180

## down: stop the stack (Postgres data is kept)
down:
	$(COMPOSE) down

## clean: stop the stack and delete its data (fresh database next `make up`)
clean:
	$(COMPOSE) down -v

## logs: follow logs, e.g. `make logs` or `make logs svc=order`
logs:
	$(COMPOSE) logs -f --tail=100 $(svc)

## ps: container status and health
ps:
	$(COMPOSE) ps

## smoke: end-to-end check against the running stack (after `make up`)
smoke:
	./scripts/smoke.sh

## gen: regenerate all generated code (commit the result)
##      protobuf contracts → gen/ (buf), SQL queries → */dbgen (sqlc, needs Docker)
gen:
	$(TOOL) buf lint
	$(TOOL) buf generate
	$(call SQLC,services/order) vet
	$(call SQLC,services/order) generate

## gen-check: fail if regenerating changes or adds any file (run on a clean tree; CI runs it)
gen-check: gen
	@changes=$$(git status --porcelain); \
	if [ -n "$$changes" ]; then \
		echo "generated code is out of date; run 'make gen' and commit the result:"; \
		echo "$$changes"; \
		exit 1; \
	fi

## test: unit tests with the race detector, in random order
test:
	go test -race -shuffle=on ./...

## test-int: unit + integration tests (needs Docker: starts real Postgres etc.)
test-int:
	go test -race -shuffle=on -tags integration ./...

## lint: golangci-lint (pinned version, no global install needed)
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

## run-gateway: run the Gateway locally with services/gateway/.env
run-gateway: $(GATEWAY_ENV)
	$(call load-env,$(GATEWAY_ENV)) && go run ./services/gateway

## run-order: run Order locally with services/order/.env (start a DB with `make db-up`)
run-order: $(ORDER_ENV)
	$(call load-env,$(ORDER_ENV)) && go run ./services/order

## migrate: apply Order's migrations using services/order/.env
migrate: $(ORDER_ENV)
	$(call load-env,$(ORDER_ENV)) && go run ./services/order migrate

## migration: new empty Order migration file, e.g. `make migration name=add_menu_items`
migration:
	@test -n "$(name)" || { echo 'usage: make migration name=<snake_case_name>'; exit 1; }
	$(TOOL) goose -s -dir services/order/db/migrations create $(name) sql

## db-up: start a throwaway Postgres on localhost:5432 (removed on db-down)
db-up:
	docker run -d --rm --name $(DB_CONTAINER) -p 5432:5432 \
		-e POSTGRES_USER=deliverit -e POSTGRES_PASSWORD=deliverit -e POSTGRES_DB=deliverit \
		$(DB_IMAGE)

## db-down: stop and remove the local Postgres (its data is gone)
db-down:
	docker stop $(DB_CONTAINER)

# A missing .env is created from its example, with a note, on first use.
$(GATEWAY_ENV) $(ORDER_ENV):
	cp $@.example $@
	@echo "created $@ from $@.example; edit it if you need different values"
