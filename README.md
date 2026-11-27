# DeliverIt

[![ci](https://github.com/Yulian302/deliverit/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Yulian302/deliverit/actions/workflows/ci.yml)

DeliverIt is a food-delivery platform written in Go. Customers order from restaurants, restaurants prepare the food, and the system sends a nearby courier, with live tracking on a map. It's a learning and portfolio project about production-grade distributed systems: idempotency, a transactional outbox, event-driven services, geo-matching and observability. Understanding every part matters more than adding features.

**Status:** slice 1 of 8, the walking skeleton. A request goes through the Gateway to the Order service and into Postgres, and the whole path appears as one trace in Grafana. CI runs lint, tests, generated-code drift checks, a vulnerability scan and an end-to-end smoke test.

![One request traced from gateway to order to Postgres](docs/img/slice1-trace.png)

## Prerequisites

- Docker with Compose v2 (`docker compose`)
- Go, at the version in [go.mod](go.mod) (only needed to develop, not to run the stack)
- `curl` and `jq` (used by `make smoke`)

## Quick start

```bash
make up      # build images and start the whole stack; returns once everything is healthy
make smoke   # end-to-end check against the running stack
make down    # stop the stack (Postgres data is kept)
make clean   # stop the stack and delete its data (fresh database next `make up`)
```

`make smoke` checks that a search returns the 10 seeded restaurants nearest first, that bad input gets a `400 application/problem+json` response, and that the request shows up in Tempo as one trace (gateway → order → postgres) with its logs in Loki. It finishes by printing the trace ID so you can open it in Grafana.

Try the API yourself:

```bash
curl -s 'http://localhost:8080/v1/restaurants?near=50.4501,30.5234' | jq
```

Log in as a demo user (no password; the users are listed in [users.go](services/gateway/internal/auth/users.go)) and use the token:

```bash
token=$(curl -s -H 'Content-Type: application/json' \
  -d '{"userId":"0193a1b1-0000-7000-8000-0000000000c1"}' \
  http://localhost:8080/v1/auth/demo-login | jq -r .token)
curl -s -H "Authorization: Bearer $token" http://localhost:8080/v1/auth/me | jq
```

`make logs` follows every service (`make logs svc=order` for one), and `make ps` shows container health.

## Local URLs

| What | URL |
|---|---|
| Gateway (public API) | http://localhost:8080 |
| Order (Connect, for debugging) | http://localhost:8081 |
| Grafana (traces, logs, metrics) | http://localhost:3000 |
| Kafka UI | http://localhost:8090 |
| OTLP gRPC / HTTP (for services run locally) | localhost:4317 / localhost:4318 |
| Postgres | localhost:5433 |
| Redis | localhost:6379 |
| Kafka | localhost:9094 |

Each service has `/livez` and `/readyz` health endpoints.

## Development

```bash
make test       # unit tests with the race detector, in random order
make test-int   # unit + integration tests (needs Docker: starts real Postgres etc.)
make lint       # golangci-lint (pinned version, no global install needed)
make gen        # regenerate protobuf (buf) and SQL (sqlc) code; commit the result
make gen-check  # fail if regenerating changes any file (CI runs this)
```

Run a service outside Docker:

```bash
make db-up        # throwaway Postgres on localhost:5432 (removed on db-down)
make migrate      # apply Order's migrations
make run-order    # Order with services/order/.env
make run-gateway  # Gateway with services/gateway/.env
make db-down      # stop and remove the local Postgres
```

The first run creates each service's `.env` from its committed `.env.example`. `make migration name=<snake_case_name>` adds a new Order migration.

## Repository layout

| Path | Contents |
|---|---|
| [services/](services/) | Deployable services: `gateway` (public HTTP API) and `order` (restaurant catalog; placing orders arrives in slice 2) |
| [platform/](platform/) | Shared building blocks: config, logging, telemetry, health, HTTP server, RPC, Postgres |
| [proto/](proto/) | Protobuf contracts between services |
| [gen/](gen/) | Code generated from `proto/` (committed; checked by `make gen-check`) |
| [api/](api/) | OpenAPI description of the public HTTP API |
| [deploy/](deploy/) | Docker Compose stack for local runs |
| [scripts/](scripts/) | End-to-end smoke test |
| [tools/](tools/) | Pinned dev tools (buf, protoc plugins, goose) in their own Go module |
| [docs/](docs/) | Design spec and implementation plans |

## Milestones

The MVP is built in thin vertical slices. Each one runs end to end before the next one starts.

| # | Slice | Status |
|---|---|---|
| 1 | Walking skeleton | ✅ Done |
| 2 | Place order + payment: idempotency keys, outbox → Kafka, async payment, circuit breaker | Next |
| 3 | Courier location: GPS ingest, Redis GEO, live fan-out | Planned |
| 4 | Simulator + ops map | Planned |
| 5 | Dispatch: offers with expiry, atomic accept, leader lease | Planned |
| 6 | Web app: customer, restaurant and courier views | Planned |
| 7 | Resilience + observability pass: timeouts, rate limiting, DLQ, dashboards, SLOs | Planned |
| 8 | Load + chaos tests, recorded numbers, ADRs | Planned |

### Slice 1: walking skeleton

One real request runs through every layer, can be followed as a single trace, and is checked automatically on every push.

`GET /v1/restaurants?near=lat,lng` goes Gateway → Order (Connect RPC) → Postgres and returns the seeded restaurants, nearest first. In Grafana it is one trace with 7 spans (see the screenshot above), and both services' log lines for it carry the same `trace_id`.

- **Shared platform ([platform/](platform/)):** environment config validated at startup, structured JSON logging with trace IDs, OpenTelemetry traces/metrics/logs over OTLP, `/livez` and `/readyz` probes, an HTTP server with request-ID and tracing middleware, graceful shutdown, a Postgres pool with migrations and readiness checks, and shared Connect RPC plumbing.
- **Order service:** owns the restaurant catalog. Migrations (table, location index, seed data), sqlc-generated queries, the `ListRestaurants` RPC, and a `migrate` subcommand.
- **Gateway:** the public HTTP API, described in [api/openapi.yaml](api/openapi.yaml). Errors are `application/problem+json`, so bad input is a 400, never a 500.
- **Contract:** the RPC is defined in protobuf ([proto/](proto/)), and Go code is generated from it with buf into [gen/](gen/).
- **Local stack:** Docker Compose with Postgres, Redis, Kafka, Kafka UI and Grafana (Tempo, Loki, Prometheus), plus both services and a one-off migration job. Every image is pinned to an exact version. `make smoke` checks the search, the 400 case, the full trace in Tempo and the logs in Loki.
- **CI:** golangci-lint, buf lint and buf breaking (catches incompatible proto changes), unit tests, integration tests against a real Postgres, a check that generated code is up to date, govulncheck, and the end-to-end smoke test. It runs once per push: branches without a PR get their own run, and a branch with an open PR is covered by the PR's run.

## Design

[docs/SPEC.md](docs/SPEC.md) is the full MVP design: services, events, state machines, failure handling, and the acceptance criteria each slice works toward.
