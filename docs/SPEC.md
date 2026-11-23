# DeliverIt — MVP Design Spec

*Status: draft for review (builder check done) · 2026-11-23*

---

## 1. Intent

DeliverIt is a food-delivery platform. Customers order from restaurants, restaurants prepare the food, and the system dispatches a nearby courier who picks it up and delivers it, with live tracking on a map.

**Why it exists:** it's a learning and portfolio project. The author wants to relearn, hands-on, how to build a production-grade distributed system in Go, and to have something concrete to walk through in interviews. **Understanding every part matters more than having many features.** Each piece should be small enough for the author to explain line by line.

**What success looks like:**
- An order flows end to end through 5 services, and a single trace shows the whole journey.
- Failures can be triggered on demand (a killed service, a duplicate request, a double tap, a courier going offline), and the system visibly does the right thing.
- Load from the simulator produces real numbers in the README: throughput, latencies, and zero correctness violations.
- The architecture can grow into the "10-week" version (§15) without being rewritten.

---

## 2. Goals, Non-Goals, Anti-Goals

**Goals**
- 5 Go services that communicate over sync RPC and async events.
- Correctness under concurrency and failure: idempotency at every layer, transactional outbox, idempotent consumers, atomic courier assignment.
- Real-time behaviour: courier GPS ingestion, live tracking over WebSocket, offers with timeouts.
- Production basics everywhere: timeouts, retries with backoff, circuit breaker, rate limiting, graceful shutdown, health checks, structured logs, metrics, distributed tracing.
- A Next.js web app with four views: customer, restaurant, courier, ops.
- A simulator that generates realistic load and failures.
- Runs locally with one command (`make up`).

**Non-goals (MVP)**
- Real payments, real users, real maps APIs, native mobile apps.
- More than one city/region. Multiple dispatch regions are an upgrade (§15).
- Kubernetes. docker-compose only for the MVP; K8s is an upgrade.
- Road routing. Straight-line distance for ETAs in the MVP; OSRM is an upgrade.
- Batch matching and order batching (upgrade).
- Restaurant menus beyond a simple flat list of items with prices.

**Anti-goals** (count as failure even if the system works)
- Code the author can't explain, or "magic" frameworks that hide the mechanics being learned (no ORM, no service mesh, no workflow engine like Temporal).
- An order that is lost, a courier assigned to two jobs, or a capture without a delivery. These are never acceptable, even under chaos.
- A UI-heavy project where the backend is an afterthought.
- Microservices that share a database or import each other's internals.

---

## 3. Constraints

- **Go 1.24+** for all backend code and the simulator.
- **Next.js (App Router, TypeScript)** for the web app.
- **Open/free only:** MapLibre GL with a free tile source that needs no paid key; OpenStreetMap data.
- Must run on one developer machine (WSL2, Docker).
- **No cross-service DB access:** each service owns its own Postgres database (one Postgres instance, separate logical databases in the MVP).

---

## 4. Approaches Considered

| Approach                          | Summary                                                                                                                                                                              | Verdict                                                                                                                       |
|-----------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------|
| **A. Orchestrated saga + events** | Order runs the order lifecycle as an explicit state machine and calls Payment synchronously. State changes go out as events (outbox → Kafka). Dispatch and Location react to events. | **Chosen.** The flow lives in one readable place, it's easy to trace and debug, and it's what interviewers usually ask about. |
| B. Pure choreography              | No coordinator; each service reacts only to events.                                                                                                                                  | Rejected as the base: the flow is spread across services and hard to follow. Worth an ADR comparing it to A.                  |
| C. Sync REST only, no broker      | Services call each other over HTTP.                                                                                                                                                  | Rejected: skips outbox, at-least-once delivery and idempotent consumers, which are core learning goals.                       |

**Web UI:** Next.js was chosen over Go + templ + htmx so the frontend is a real showcase as well.

---

## 5. Domain Model

### Glossary
| Term           | Meaning                                                                                                      |
|----------------|--------------------------------------------------------------------------------------------------------------|
| **Customer**   | Places orders, tracks delivery.                                                                              |
| **Restaurant** | Has a location, a menu and a typical prep time. Accepts or rejects orders and marks them ready.              |
| **Courier**    | Delivers. Has a live position and an availability state.                                                     |
| **Order**      | What the customer bought, from one restaurant, to one drop-off address. Owned by **Order**.                  |
| **Payment**    | Authorization/capture/void of the order amount. Owned by **Payment**.                                        |
| **Job**        | The delivery task for one order: pick up at the restaurant, drop off at the customer. Owned by **Dispatch**. |
| **Offer**      | A time-limited proposal of a Job to one courier. Owned by **Dispatch**.                                      |
| **Position**   | A courier's latest GPS point (`lat, lng, seq, ts`). Owned by **Location**.                                   |

### State machines

**Order** (Order service)
```
PENDING_PAYMENT ──auth ok──▶ AWAITING_RESTAURANT ──accept──▶ PREPARING ──ready──▶ READY
      │                            │       │                     │                   │
   auth fail                   reject /2-min timeout /       (job assigned      picked up
      ▼                       customer cancel                 meanwhile)            ▼
   FAILED                          ▼                                            PICKED_UP
                               CANCELLED (void auth)                               │
                                                                                delivered
   PREPARING or READY ──job unassignable (10 min)──▶ CANCELLED (void auth)         ▼
                                                                         DELIVERED (capture)
```
- The customer can cancel only in `PENDING_PAYMENT` or `AWAITING_RESTAURANT`.
- Courier assignment is tracked on the **Job**, not as an Order state. Order keeps a denormalized `courier_id` for display, updated from `job.*` events.

**Job** (Dispatch service)
```
PENDING ─(dispatch time reached)─▶ OFFERING ─accepted─▶ ASSIGNED ─arrived─▶ AT_PICKUP ─picked up─▶ PICKED_UP ─delivered─▶ DELIVERED
                                     │  ▲                  │
                                     │  └─declined/expired─┘ (courier went offline before pickup → back to OFFERING)
                                     └─ no courier after 10 min ──▶ UNASSIGNABLE
any state before PICKED_UP ─order cancelled─▶ CANCELLED
```

**Courier** (Dispatch service, with liveness from Location)
```
OFFLINE ⇄ IDLE ─offer sent─▶ OFFERED ─accept─▶ ON_JOB ─delivered─▶ IDLE
                    ▲            │
                    └─decline/expire─┘
any ─heartbeat lost 90s─▶ OFFLINE (active pre-pickup job goes back to OFFERING)
```

**Offer** (Dispatch service): `PENDING → ACCEPTED | DECLINED | EXPIRED | CANCELLED`. TTL 20s.

**Payment** (Payment service): `AUTHORIZED → CAPTURED | VOIDED`, or `FAILED` when authorization fails.

---

## 6. Architecture

```
 Next.js web (customer / restaurant / courier PWA / ops)        Simulator (Go)
            │ REST (OpenAPI)          │ WebSocket                    │ same REST + WS as real clients
            ▼                         ▼                              ▼
   ┌───────────────────────── Gateway ──────────────────────────────────┐
   │ auth (JWT), rate limit, request IDs, deadlines, REST→RPC, WS hub   │
   └────┬──────────────┬────────────────┬──────────────────┬───────────────┘
        │ Connect-RPC  │ Connect-RPC    │ Kafka: courier.gps│ Redis Pub/Sub: live positions
        ▼              ▼                ▼                  │ Kafka: events for UI push
   ┌─────────┐    ┌──────────┐   ┌──────────┐              │
   │  Order  │───▶│ Payment  │   │ Location │──────────────┤
   │ catalog │RPC │ mock PSP │   │ Redis GEO│              │
   │ + saga  │    └──────────┘   └────┬─────┘              │
   └────┬────┘                        │ RPC: nearby couriers
        │ outbox                 ┌────▼─────┐              │
        ▼                        │ Dispatch │──────────────┘
   ═══════════ Kafka (durable events, keyed by aggregate ID) ═══════════
```

### Services

| Service      | Owns (data)                                                                   | Talks to                                           | Notes                                                                                                                          |
|--------------|-------------------------------------------------------------------------------|----------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------|
| **Gateway**  | demo identities (seeded users), rate-limit counters (Redis)                   | all services via Connect-RPC; Kafka; Redis Pub/Sub | The only public entry point. Stateless, so it can be scaled horizontally. Holds the WebSocket connections.                     |
| **Order**    | restaurants, menu items, orders, order items, outbox, inbox, idempotency keys | Payment (RPC), Kafka                               | Saga orchestrator. Restaurant catalog lives here in the MVP (split out in the upgrade).                                        |
| **Payment**  | payments (mock PSP)                                                           | —                                                  | Configurable failure rate, latency and timeouts, to make the chaos demos possible.                                             |
| **Location** | latest positions (Redis), courier liveness, outbox (small Postgres DB)        | Kafka, Redis Pub/Sub                               | Ingests GPS, rejects stale updates, answers "couriers near X", publishes positions and online/offline events.                  |
| **Dispatch** | couriers, jobs, offers, outbox, inbox                                         | Location (RPC), Kafka                              | Matching, offers, atomic assignment, job lifecycle. Runs with a **leader lease** (§8.6): one active instance and hot standbys. |

### Communication rules
- **Sync (Connect-RPC, protobuf):** used only for queries and for a call whose result the caller needs right now: Order→Payment, Dispatch→Location, and Gateway→everyone.
- **Async events (Kafka, durable, at-least-once):** used for every state change other services care about. Always published through the **outbox**, never directly from a request handler. **The record key is the aggregate ID** (order ID, job ID, courier ID), so all events of one aggregate land in the same partition, in order.
- **High-volume ingest (Kafka):** raw GPS goes through a Kafka topic with short retention. It's buffered and replayable, and Location can scale out as a consumer group.
- **Lossy real-time fan-out (Redis Pub/Sub):** throttled live positions from Location to every Gateway instance. Nothing is stored; a missed position is replaced by the next one within a second. Redis is used here instead of Kafka because every Gateway instance needs every message and none of them need history.

### Kafka topics
| Topic             | Key            | Events                                                                                                                                               | Producer  | Consumers (group)                                                                             |
|-------------------|----------------|------------------------------------------------------------------------------------------------------------------------------------------------------|-----------|-----------------------------------------------------------------------------------------------|
| `orders.events`   | order ID       | `order.placed`, `order.payment_authorized`, `order.accepted`, `order.ready`, `order.picked_up`, `order.cancelled`, `order.delivered`, `order.failed` | Order     | Order payment worker (`order-payment`), Dispatch (`dispatch`), Gateway (`gateway-<instance>`) |
| `dispatch.events` | job ID         | `job.created`, `job.assigned`, `job.picked_up`, `job.delivered`, `job.unassignable`, `offer.created`, `offer.expired`, `offer.cancelled`             | Dispatch  | Order (`order`), Gateway (`gateway-<instance>`)                                               |
| `couriers.events` | courier ID     | `courier.online`, `courier.offline`                                                                                                                  | Location  | Dispatch (`dispatch`)                                                                         |
| `courier.gps`     | courier ID     | raw GPS `{lat, lng, seq, ts}`                                                                                                                        | Gateway   | Location (`location`)                                                                         |
| `<topic>.dlq`     | same as source | messages that kept failing (§8.2)                                                                                                                    | consumers | ops (inspected by hand)                                                                       |

- Partitions: 6 per topic in the MVP (one broker, KRaft mode, no ZooKeeper). Retention: 7 days for event topics, 1 hour for `courier.gps`.
- The event type travels in a header (`event-type`). The envelope (protobuf) is `id (UUIDv7), type, occurred_at, aggregate_id, aggregate_version, payload`. Trace context travels in Kafka headers (§9.5).
- **Gateway consumes as broadcast:** each Gateway instance uses its own consumer group (`gateway-<instance-id>`) starting from the latest offset, because every instance must see every event to push it to whichever WebSocket it holds. Anything an instance missed while down is recovered on reconnect (§7.6).

Redis Pub/Sub channels: `pos.courier.<courierId>` (Location → Gateway, throttled to ≤1/s per courier), `pos.ops` (sampled positions of all couriers for the ops map).

---

## 7. Key Flows

### 7.1 Place an order
1. Customer: `POST /v1/orders` with an `Idempotency-Key` header.
2. Gateway checks the JWT and rate limit, then forwards to `Order.PlaceOrder` with the key and a deadline.
3. Order, in **one transaction**: inserts the idempotency key row (a concurrent duplicate blocks on the unique index until this transaction commits, then replays the stored response; see §8.1), inserts the order as `PENDING_PAYMENT`, stores the response, writes `order.placed` to the outbox.
4. Response `202 Accepted` with the order in `PENDING_PAYMENT`. **The request does not wait for payment.** The client follows progress over `/ws/track/{orderId}` (or by polling `GET /v1/orders/{id}`).
5. **Payment step (asynchronous, inside Order):** a payment worker in Order picks up orders in `PENDING_PAYMENT` (driven by its own `order.placed` event, plus a periodic sweep as a safety net) and calls `Payment.Authorize(payment_key = order_id)` (2s timeout per attempt, up to 3 attempts with backoff + jitter, behind a circuit breaker).
   - OK → `AWAITING_RESTAURANT` + outbox event.
   - Declined → `FAILED` + outbox event.
   - **Unknown (timeout or breaker open)** → the order stays `PENDING_PAYMENT`. The sweep later calls `Payment.GetPayment(order_id)` first and only re-authorizes (same key) if no payment exists. Never treat "unknown" as "failed". An order still `PENDING_PAYMENT` after 5 min is moved to `FAILED`, with a void as a safety measure.

### 7.2 Restaurant
- The restaurant view gets new orders over WebSocket and accepts (`PREPARING`, including a prep time estimate) or rejects (`CANCELLED` + void).
- If there's no response within 120s, the order is auto-cancelled and the authorization voided.
- The restaurant marks the order `READY`.

### 7.3 Dispatch
1. Dispatch consumes `order.accepted` and creates a Job in `PENDING` with `dispatch_at = accepted_at + prep_time − est_travel_time` (never earlier than now).
2. When the time comes (a scheduler loop in the leader), the Job moves to `OFFERING`.
3. **Candidates:** `Location.NearbyCouriers(restaurant, radius)` returns couriers in radius steps of 1 → 2 → 4 km, filtered to `IDLE` couriers who haven't already been skipped for this job.
4. **Score** (MVP): the lowest straight-line ETA to the restaurant wins. Weights live in config.
5. **Offer:** creates the Offer (TTL 20s), sets the courier to `OFFERED`, writes `offer.created` to the outbox. The Gateway pushes it to the courier's WebSocket.
6. **Accept** (`POST /v1/offers/{id}/accept` with an idempotency key), in one transaction with conditional updates:
   - `UPDATE offers SET status='ACCEPTED' WHERE id=$1 AND status='PENDING' AND expires_at > now()`
   - `UPDATE couriers SET state='ON_JOB', job_id=$2 WHERE id=$3 AND state='OFFERED'`
   - `UPDATE jobs SET status='ASSIGNED', courier_id=$3 WHERE id=$2 AND status='OFFERING'`
   - If any of them affects 0 rows, the whole transaction rolls back and the client gets `409 offer_no_longer_valid`.
7. Decline or expiry → the courier goes back to `IDLE`, is added to the skip list, and the next candidate is tried.
8. No courier after 10 min → `UNASSIGNABLE` → Order cancels and voids the authorization.

### 7.4 Pickup and delivery
The courier taps "Arrived", "Picked up", "Delivered" (each idempotent). Dispatch moves the Job and emits events. Order follows: `PICKED_UP`, then `DELIVERED`, which triggers `Payment.Capture(order_id)`.

**Pickup requires the food to be ready.** Dispatch consumes `order.ready` and sets `jobs.food_ready = true`. "Picked up" is rejected with `409 food_not_ready` until then. The courier UI shows "Waiting for food" at the restaurant. So Order only ever sees `READY → PICKED_UP`.

**Unassignable timer:** a job has an `offering_deadline` set when it first enters `OFFERING` (+10 min). Declines and expired offers do *not* reset it. If an assigned courier drops out before pickup (§7.5), the job goes back to `OFFERING` with a **new** 10-minute deadline, because the delay wasn't the customer's or restaurant's fault.

**Cancellation after the food is prepared** (unassignable job): the authorization is voided and the customer isn't charged. Compensating the restaurant is out of scope for the MVP (§2).

### 7.5 Location and live tracking
1. The courier app sends `{lat, lng, seq, ts}` every 3s over `/ws/courier`.
2. The Gateway produces it to Kafka topic `courier.gps` (key = courier ID, `acks=1`, small linger for batching; a lost point is acceptable).
3. Location (consumer group `location`) drops the update if `seq ≤ last_seq` (handles out-of-order and duplicate updates), otherwise updates Redis (`GEOADD` + a per-courier hash with `seq`, `ts`, `last_seen`), and publishes to Redis Pub/Sub `pos.courier.<id>`, throttled to ≤1/s.
4. Customer `/ws/track/{orderId}`: the Gateway checks ownership, gets the order's courier from `job.assigned` (or `Dispatch.GetJob`), subscribes to `pos.courier.<id>` plus that order's events, and pushes them to the browser.
5. **Liveness:** a sweeper in Location marks couriers *suspect* after 30s without GPS and *lost* after 90s, and emits `courier.offline` (lost) / `courier.online` (GPS seen again after being lost) through the outbox to `couriers.events`. (Location therefore also has a small Postgres database for its outbox and the liveness state it reports.)

**Who decides availability.** Two separate inputs, combined in **Dispatch**, which is the only owner of courier state:
- **Intent** (`POST /v1/courier/online|offline`): the courier's choice. Owned by Dispatch.
- **Liveness** (`courier.online/offline` events): whether GPS is arriving. Reported by Location.

A courier is **available (`IDLE`) only when intent = online and liveness = live.** Consequences:
- GPS returns after a loss and intent is still online → the courier goes back to `IDLE` automatically.
- The courier taps offline but GPS keeps arriving → `OFFLINE` (intent wins). Location still tracks the position; it just isn't used for matching.
- Going offline (by tap or by GPS loss) while `OFFERED` → the offer becomes `CANCELLED` and the job moves on to the next candidate.
- GPS lost while `ON_JOB` before pickup → the job goes back to `OFFERING` (new deadline, §7.4) and the courier is unassigned. After pickup → the job stays with the courier and `order_stuck` alerts ops (the food is with them; reassigning makes no sense).
- Tapping offline while `ON_JOB` is rejected (`409 job_in_progress`).

### 7.6 WebSocket reconnect
Each order/job WebSocket message carries the aggregate's `version`. On reconnect the client sends the last version it saw. The Gateway then asks the **owning service** for what was missed (`Order.GetOrderEvents(orderId, afterVersion)`, `Dispatch.GetJob` / `Dispatch.GetPendingOffers(courierId)`) and sends it before resuming the live stream. Kafka isn't used for this, because it can't efficiently replay one order's events out of a whole partition; each owning service keeps an `events` history table alongside its outbox. Positions are never replayed; only the latest one is sent.

---

## 8. Correctness Guarantees

### 8.1 Idempotency, at three layers
| Layer              | Mechanism                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
|--------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Public API         | An `Idempotency-Key` header is **required** on every mutating endpoint. Keys are stored by the **owning service** (not the Gateway) in the *same transaction* as the business change: `(scope=user_id, key, request_hash, response_code, response_body, created_at)`, kept for 24h. Every mutating endpoint does its work inside a single DB transaction (slow work such as payment happens asynchronously, §7.1), so concurrency needs no "in progress" state: the first request inserts the key row; **concurrent duplicates block on the unique index until the first commits, then read and replay its stored response.** If the first transaction rolls back, a duplicate proceeds as if it were the first. The same key with a different `request_hash` returns `422`. The replayed response is the original one (e.g. the `PENDING_PAYMENT` snapshot), not the current state. |
| Service to service | Payment is keyed by `order_id` for authorize, capture and void. Repeating a call returns the original result.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| Event consumers    | An **inbox** table `processed_messages(consumer, message_id)` is written in the same transaction as the side effect. Duplicates are acknowledged and skipped.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| GPS stream         | `seq` per courier; last-write-wins on `seq`, not on arrival time.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |

### 8.2 Transactional outbox
Order, Dispatch and Location write events to an `outbox` table in the same transaction as the state change. Each outbox row also stores the **trace context** of the transaction that wrote it (§9.5).

**Relay:** one relay per service (a goroutine that holds a Postgres advisory lock, so even with several instances only one relays). It reads unsent rows in `id` order with `SELECT ... FOR UPDATE SKIP LOCKED`, produces them to Kafka with **key = aggregate ID**, waits for the broker's ack (`acks=all`, idempotent producer on), then marks the rows sent. If it crashes between producing and marking, the rows are produced again on restart. Kafka's idempotent producer only removes duplicates within one producer session, so **consumers must deduplicate** (inbox, §8.1).

**Per-aggregate ordering:**
- Same key → same partition, and a single relay produces in commit order, so each aggregate's events arrive in version order.
- Consumers process **each partition sequentially** (partitions run in parallel with each other). The offset is committed **only after** the consumer's DB transaction (side effect + inbox row) commits. So a crash causes redelivery, never loss.
- Consumers track `last_applied_version` per aggregate. `version ≤ last` → duplicate: skip and commit. `version == last + 1` → apply. `version > last + 1` → a gap, which should be impossible given the above, so it's treated as an error (see below). Never skip an event silently.

**Failures while consuming:** transient errors (DB down, RPC timeout) → retry in place with exponential backoff, which pauses that partition. After 5 attempts, or immediately for a message that can't be parsed → the message goes to `<topic>.dlq` with the error in its headers, the partition moves on, and `dlq_messages_total` alerts ops. *(Upgrade: retry topics with delays, so one bad aggregate doesn't block its partition.)*

### 8.3 Atomic assignment
Conditional updates in one transaction (§7.3 step 6). This is the only way a courier becomes `ON_JOB`.

### 8.4 State transitions
Every state change is a conditional `UPDATE ... WHERE status = <expected>`. A transition that doesn't apply is logged and counted, never forced.

### 8.5 Payment uncertainty
"Unknown" is a separate outcome from "failed". A reconciler resolves unknown payments by querying Payment. Capture happens only on `DELIVERED`, and void only on cancellation.

### 8.6 Dispatch leader lease
Each Dispatch instance tries to get a Postgres advisory lock on its own connection (`pg_try_advisory_lock(<region id>)`). The holder runs the scheduler, the offer-expiry sweeper and the event consumers. Standbys retry every 2s. If the leader dies, its DB session ends, the lock is released, and a standby takes over within a few seconds. RPC reads (`GetJob`) are served by every instance. *(Upgrade: one lease per region, so regions are spread across instances.)*

### 8.7 Correctness counters (must stay at 0)
`double_assignment_total`, `capture_without_delivery_total`, `order_stuck_total` (no progress for longer than the maximum expected time in its state), `non_idempotent_retry_total` (a non-idempotent call was retried; should never happen).

---

## 9. Production-Grade Behaviour

### 9.1 Timeouts (defaults, all configurable)
| Where                                              | Value                                             |
|----------------------------------------------------|---------------------------------------------------|
| Gateway request deadline (passed down via context) | 5s                                                |
| `http.Server` ReadHeaderTimeout / IdleTimeout      | 5s / 60s                                          |
| Order → Payment per attempt                        | 2s, 3 attempts, exponential backoff + full jitter |
| Dispatch → Location query                          | 500ms                                             |
| DB statement                                       | 1s (`context`)                                    |
| Offer TTL                                          | 20s                                               |
| Restaurant accept window                           | 120s                                              |
| Courier suspect / offline                          | 30s / 90s                                         |
| Job unassignable                                   | 10 min                                            |
| Graceful shutdown grace                            | 20s                                               |

### 9.2 Resilience
- Retries only on idempotent operations, and only on retryable errors (`Unavailable`, `DeadlineExceeded`).
- Circuit breaker on Order → Payment (`sony/gobreaker`).
- Rate limiting in the Gateway: token bucket per `(user, route group)` in Redis. Returns `429` with `Retry-After`.
- Bounded concurrency for consumers: one goroutine per assigned partition, with `max.poll.records` / fetch sizes bounded.
- WebSocket backpressure: a per-connection send buffer. Slow clients are disconnected, not allowed to block the hub.

### 9.3 Graceful shutdown (every service)
On SIGTERM: `/readyz` starts failing → stop accepting new connections → finish in-flight HTTP/RPC requests (`Server.Shutdown`) → stop consumers after the current message, commit offsets, leave the consumer group cleanly → stop the outbox relay → release the lease (Dispatch) → close WebSockets with a "reconnect" close code (Gateway) → flush OpenTelemetry → close DB/Redis/Kafka clients (flush the producer first). All of this must finish within the grace period.

### 9.4 Health
`/livez` (the process is up) and `/readyz` (dependencies reachable and not draining) on every service.

### 9.5 Observability
- **Tracing:** OpenTelemetry on HTTP, Connect-RPC, pgx, Redis and **Kafka records** (`franz-go` `kotel` plugin).
  - **Async hops continue the trace:** the outbox row stores the trace context of the transaction that wrote it, the relay sets it on the Kafka record headers, and the consumer's span is a *child* of it. So one user action plus everything it sets off (e.g. place order → payment → `order.placed` → Dispatch) is **one connected trace**.
  - **Separate user actions are separate traces** (restaurant accept, courier accept, pickup...), because they're separate requests minutes apart. They are tied to the order by an `order.id` span attribute on every span, plus a **span link** to the order's original trace (its trace context is stored on the order and the job).
  - Scheduler-started work (dispatch timer, sweeps) starts a new trace with a link to the order's original trace.
- **Logs:** `log/slog` JSON with `trace_id`, `span_id`, `request_id`, `service`.
- **Metrics:** RED per endpoint/RPC. Business: orders by state, time-to-assign, offer acceptance rate, delivery duration, ETA error. System: GPS ingest rate, WS connections, outbox lag, consumer lag. Plus the correctness counters (§8.7).
- **Stack:** OTel SDK → `grafana/otel-lgtm` (one container with Collector, Tempo, Prometheus, Loki and Grafana) for local dev. Dashboards are checked into the repo.
- **SLOs (MVP):** 99% of `POST /orders` < 500ms (achievable because payment is asynchronous, §7.1); 99% of orders leave `PENDING_PAYMENT` within 5s when Payment is healthy; p95 time-to-assign < 60s under nominal simulator load.

### 9.6 Security
- JWT (EdDSA) issued by the Gateway at demo login (pick a seeded user). Roles: `customer`, `restaurant`, `courier`, `ops`. Every resource checks ownership (a customer sees only their own orders, a courier only their own offers).
- Internal RPC sits on a private Docker network. *(Upgrade: mTLS.)*
- Input validation at the Gateway (and again in the owning service for invariants). Only parameterized SQL (`sqlc`).
- Secrets only via env vars; nothing committed. `govulncheck` in CI.

---

## 10. APIs (sketch)

**Public REST (Gateway, OpenAPI 3 in `api/openapi.yaml`, TS types generated for the web app):**
```
POST /v1/auth/demo-login                 {userId} → {token}
GET  /v1/restaurants?near=lat,lng
GET  /v1/restaurants/{id}/menu
POST /v1/orders                          (Idempotency-Key)
GET  /v1/orders/{id}
POST /v1/orders/{id}/cancel              (Idempotency-Key)
POST /v1/restaurant/orders/{id}/accept   (Idempotency-Key) {prepMinutes}
POST /v1/restaurant/orders/{id}/reject   (Idempotency-Key)
POST /v1/restaurant/orders/{id}/ready    (Idempotency-Key)
POST /v1/courier/online | /offline
POST /v1/offers/{id}/accept | /decline   (Idempotency-Key)
POST /v1/jobs/{id}/arrived | /picked-up | /delivered   (Idempotency-Key)
GET  /v1/ops/snapshot                    couriers, jobs, orders counts
WS   /ws/courier            in: GPS · out: offers, job updates
WS   /ws/track/{orderId}    out: order events, courier position
WS   /ws/restaurant         out: new orders, order events
WS   /ws/ops                out: all courier positions (sampled), job events
```
Errors use RFC 9457 problem+json with a stable `code` field.

**Internal RPC (protobuf in `proto/`, managed with `buf`):** `OrderService`, `PaymentService` (Authorize, Capture, Void, GetPayment), `LocationService` (NearbyCouriers, GetPosition), `DispatchService` (GetJob, GetCourier, plus the courier action commands called by the Gateway).

---

## 11. Web App (Next.js)

One Next.js app (App Router, TypeScript) under `web/`, talking **only** to the Gateway.

| Route         | Who        | What                                                                                                                                                      |
|---------------|------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `/`           | customer   | restaurants near a chosen address, menu, cart, checkout, order page with live map tracking                                                                |
| `/restaurant` | restaurant | incoming orders queue, accept/reject with prep time, mark ready                                                                                           |
| `/courier`    | courier    | mobile-first PWA: go online, see an offer with a countdown, accept/decline, job steps, sends GPS (browser geolocation or a "drive along route" fake mode) |
| `/ops`        | ops        | live city map with every courier (coloured by state), jobs, counters, and links to Grafana                                                                |
| `/login`      | all        | pick a demo user and role                                                                                                                                 |

Maps: MapLibre GL with a free, keyless vector tile source.

---

## 12. Simulator

A Go program (`simulator/`) that uses the **same public REST + WS API** as the real clients, so it exercises the real paths.
- Seeds or uses N restaurants and N couriers inside the city bounding box.
- Couriers: go online, send GPS every 3s, move in straight lines at a configurable speed with some jitter, accept offers with probability `p` after a random delay, sometimes go silent (to exercise the offline path).
- Restaurants: accept after a random delay, sometimes reject, mark ready after the prep time.
- Customers: place orders following a rate curve (configurable, with lunch/dinner peaks), and sometimes retry a request with the same idempotency key on purpose.
- **Scenarios** (flags/config): `steady`, `peak`, `chaos-payment` (makes Payment slow/failing via its admin config), `double-tap` (concurrent accepts), `courier-dropout`.
- Exports its own metrics (requests sent, errors seen, end-to-end order latency).

Real users in the web app and simulated ones coexist in the same run.

---

## 13. Repository Layout & Tooling

```
deliverit/
├── go.mod                       # single Go module (my call: simpler tooling; boundaries enforced below)
├── proto/                       # protobuf + buf.yaml
├── api/openapi.yaml             # public REST contract
├── platform/                    # shared, generic only: config, logging, otel, pg, kafka, outbox, inbox,
│                                #   idempotency, httpx (server+middleware), shutdown, health
├── services/
│   ├── gateway/  {main.go, internal/...}
│   ├── order/    {main.go, internal/..., db/migrations, db/queries}
│   ├── payment/
│   ├── location/
│   └── dispatch/
├── simulator/
├── web/                         # Next.js app
├── deploy/compose/              # docker-compose.yml, grafana dashboards, otel config
├── docs/ SPEC.md, adr/
├── Makefile                     # up, down, gen, lint, test, test-int, sim, load
└── .github/workflows/ci.yml
```
- **Boundaries:** each service's code lives under `services/<svc>/internal/`, so Go itself prevents other services from importing it. `platform/` must not import from `services/`. This is enforced with `depguard` in golangci-lint.
- **Tools:** `sqlc` + `pgx/v5`, `goose` migrations (run as a step before the service starts), `buf` + Connect-RPC, `twmb/franz-go` (Kafka client, with `kadm` for topic setup and `kotel` for tracing), `go-redis`, `slog`, OTel SDK, `golangci-lint`, `testcontainers-go`, `k6` (optional, for raw HTTP load). Go tools are pinned with `go get -tool`.
- **Inside a service:** handlers (RPC/consumer) → service (domain logic, depends on small interfaces) → store (sqlc). The service layer never imports transport or DB packages.

### Testing strategy
| Level       | What                                                                                                                                                                        |
|-------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Unit        | state machines (table tests over every transition), scoring, idempotency hashing, seq handling                                                                              |
| Integration | per service against real Postgres/Redis/Kafka via testcontainers: outbox relay, inbox deduplication, conditional assignment under **concurrent goroutines**, lease failover |
| Contract    | `buf breaking` against `main`; OpenAPI lint                                                                                                                                 |
| End to end  | compose up + simulator `steady` scenario for N orders; asserts all orders reach a terminal state and the correctness counters are 0                                         |
| Chaos       | scripted scenarios (§14)                                                                                                                                                    |

CI: lint, `go test -race -shuffle=on`, integration tests, `buf lint/breaking`, `govulncheck`, Docker image builds, a short e2e run.

---

## 14. MVP Acceptance Criteria

1. `make up` starts the 5 services, Postgres, Redis, Kafka (single broker, KRaft mode), a Kafka UI (e.g. Redpanda Console or kafka-ui) for inspecting topics, otel-lgtm and the web app. `make sim` starts the simulator.
2. Steady load: 500 couriers, 50 restaurants, ~2 orders/s for 15 minutes. All orders reach a terminal state; the correctness counters are 0; the SLOs in §9.5 hold.
3. **Tracing:** placing an order produces one connected trace: Gateway → Order → (outbox) → Kafka → Order payment worker → Payment, and → Dispatch job creation. The offer-accept trace connects Gateway → Dispatch → Kafka → Order and the Gateway WebSocket push. Searching Tempo for `order.id` shows every trace for that order, linked to the original.
4. Each of these works and is shown in the README:
   - **Duplicate `POST /orders`** (same key, 10 concurrent requests) → one order, 10 identical `202` responses.
   - **Courier double tap / two couriers racing** → exactly one assignment, the others get `409`.
   - **Payment slow or failing** → retries, the breaker opens, orders stay `PENDING_PAYMENT` and get reconciled; no double authorization.
   - **Kill the Dispatch leader** → the standby takes over in under 10s; no job is lost or assigned twice.
   - **Courier goes silent** → offline after 90s, the job is re-offered, the order is still delivered.
   - **SIGTERM any service under load** → no failed in-flight requests; consumers resume without duplicate side effects.
5. Grafana dashboards show RED metrics, business metrics and correctness counters.
6. The README explains the architecture, links to the ADRs, and includes the load and chaos results.

---

## 15. Upgrade Path ("10-week" architecture)

The MVP is designed so each of these is an addition, not a rewrite:

| Upgrade                                                                  | Built on                                                                                       |
|--------------------------------------------------------------------------|------------------------------------------------------------------------------------------------|
| Split **Catalog** out of Order                                           | Catalog is already a separate package inside Order and accessed through an interface           |
| Split a dedicated **Realtime Gateway** out of the Gateway                | The WS hub is already its own package and depends only on Kafka and Redis Pub/Sub              |
| **Multiple regions**, one lease per region, Dispatch scaled horizontally | Regions already exist in the model (MVP has one), and leases are already keyed by region       |
| **H3** cell index instead of Redis GEO                                   | `NearbyCouriers` RPC hides the index                                                           |
| **Batch matching** (Hungarian algorithm)                                 | The matcher is behind a `Matcher` interface; greedy is the first implementation                |
| **Order batching** (2 orders per courier)                                | A Job carries a list of stops in the schema from day one, even though the MVP has only 2 stops |
| **OSRM** routing and real ETAs                                           | `ETAEstimator` interface; straight-line is the first implementation                            |
| Location **history** in TimescaleDB/ClickHouse                           | Location already publishes positions; add a consumer                                           |
| **Kubernetes** (k3d + Helm, HPA on Gateway, PDBs, preStop)               | Health and shutdown behaviour already meet K8s expectations                                    |
| mTLS between services, surge pricing, ETA accuracy model                 | —                                                                                              |

---

## 16. Left to the Builder

- Library choices within the constraints above, and exact table schemas.
- Scoring weights, radius steps, and the precise throttling implementation.
- Visual design of the web app (should be clean and responsive; no design system mandated).
- Choice of free tile source and the default city (any city with good OSM coverage).
- ADR wording. At minimum write ADRs for: orchestration vs choreography, Kafka vs NATS JetStream (and why Redis Pub/Sub for live positions), outbox design, leader lease, Redis GEO vs H3, single Go module vs multiple.

## 17. Working Agreement (learning goal)

Because the point is to relearn the skills, the implementation plan should:
- Build in thin vertical slices. The first one is a walking skeleton: Gateway → Order → Postgres, traced, in compose, with CI.
- Keep each task small enough for the author to read and understand fully before moving on.
- Explain the *why* of each pattern when it's first introduced (outbox, inbox, lease, etc.), with a link to the ADR.
