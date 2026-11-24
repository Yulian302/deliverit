# DeliverIt — developer entry points.
GOLANGCI_LINT_VERSION := v2.15.0

.PHONY: test test-int lint

## test: unit tests with the race detector, in random order
test:
	go test -race -shuffle=on ./...

## test-int: unit + integration tests (needs Docker: starts real Postgres etc.)
test-int:
	go test -race -shuffle=on -tags integration ./...

## lint: golangci-lint (pinned version, no global install needed)
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
