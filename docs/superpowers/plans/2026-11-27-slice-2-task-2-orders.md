# Slice 2, Task 2 — Menu, Orders and Idempotency Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A signed-in customer reads a restaurant's menu and places an order with an `Idempotency-Key`. However many times the request is retried or raced, exactly one order exists, and every duplicate gets the original response. The customer can then read the order.

**Architecture:** Order gets a new `internal/ordering` package (model → service → store → rpc, same layering as `catalog`); the menu read goes into `catalog`. Placing an order is one Postgres transaction: insert the idempotency key row, price the lines from the database, insert the order and its items, store the response snapshot on the key row. A concurrent duplicate blocks on the key's primary key until the first transaction commits, then replays the stored snapshot (SPEC §8.1). The Gateway maps the new RPCs to REST and enforces the customer role; Order enforces it again and checks ownership.

**Tech Stack:** Go 1.26, Connect-RPC, protobuf + buf, Postgres 17, pgx/v5, sqlc, goose, testcontainers.

**Spec:** `docs/SPEC.md` §5 (Order state machine), §7.1 steps 1–4 (place order; payment is task 4), §8.1 (idempotency, public API layer), §8.4 (conditional transitions, used from task 4 on), §9.6 (ownership), §10 (`GET /v1/restaurants/{id}/menu`, `POST /v1/orders`, `GET /v1/orders/{id}`). Builds on task 1 (`platform/identity`, `api.RequireRole`).

## Global Constraints

- Everything in the slice 1 plan's Global Constraints and the task 1 plan's still applies.
- **Commit messages carry no `Co-Authored-By` trailer** (author's instruction).
- Not in this task: outbox and Kafka (task 3), payment (task 4), cancel (task 5). Orders stay in `PENDING_PAYMENT`. Task 3 adds the outbox write to the same transaction through a new store method; nothing here should make that hard.
- Money is integer minor units (kopecks, `int64`), currency `UAH`. Prices come only from the database, never from the client. An order keeps a snapshot of each item's name and price.
- IDs: orders get UUIDv7 from the app (`uuid.NewV7`); seeded menu items have fixed IDs.
- Idempotency key: required, 1–64 characters from `[A-Za-z0-9_-]`, scoped to the customer's user ID. Same key + same request → original response; same key + different request → `ALREADY_EXISTS` → Gateway `422 idempotency_key_reused`. Keys are kept 24h (SPEC §8.1); deleting older rows is task 4's sweeper — until then rows are never deleted.
- Order statuses, exactly as SPEC §5: `PENDING_PAYMENT, AWAITING_RESTAURANT, PREPARING, READY, PICKED_UP, DELIVERED, CANCELLED, FAILED`.
- Order validation (in `ordering.Service`; all → `INVALID_ARGUMENT`): restaurant exists; 1–20 lines; quantity 1–20; no menu item twice; every item exists, belongs to that restaurant and is available; delivery lat/lng in range and within **10 km** of the restaurant (great-circle).
- Who may read an order: its customer; staff of its restaurant; `ops`. Anyone else gets `NOT_FOUND` (never reveal that the ID exists).
- Gateway: request body max **16 KiB**; problem codes added: `idempotency_key_reused` (422).

## Review Focus

1. **10 concurrent identical requests with one key** → one order row, ten identical responses (SPEC §14 acceptance). → `TestPlaceOrder_ConcurrentDuplicatesCreateOneOrder` (integration).
2. **First attempt fails, retry succeeds:** a request rejected inside the transaction (unavailable item) must not burn the key; the corrected retry with the same key creates the order. → `TestPlaceOrder_FailedAttemptDoesNotKeepKey`.
3. **Same key, different body** (another quantity) → 422, and still one order. → `TestPlaceOrder_KeyReuseWithDifferentRequest`.
4. **Two customers, same key** → two independent orders (keys are per customer). → `TestPlaceOrder_KeysAreScopedPerCustomer`.
5. **Reading someone else's order** → 404, same as a missing ID; restaurant staff see only their restaurant's orders. → `TestGetOrder_Visibility`.

---

### Task 2: Menu, PlaceOrder with idempotency, GetOrder

**Files:**
- Create: `services/order/db/migrations/00004_menu_items.sql`, `00005_seed_menu.sql`, `00006_orders.sql`
- Create: `services/order/db/queries/ordering.sql`; generated `services/order/internal/ordering/dbgen/`
- Create: `services/order/internal/ordering/model.go`, `service.go`, `store.go`, `rpc.go`, `service_test.go`, `rpc_test.go`, `store_integration_test.go`
- Modify: `services/order/sqlc.yaml` (second package), `services/order/db/queries/catalog.sql` + `internal/catalog/{model,service,store,rpc}.go` (+ tests) for the menu
- Modify: `proto/deliverit/order/v1/order.proto`, regenerated `gen/`
- Modify: `services/order/main.go` (one server type serving both packages)
- Create: `services/gateway/internal/api/orders.go`, `menu.go`, `orders_test.go`, `menu_test.go`
- Modify: `services/gateway/internal/api/problem.go`, `restaurants.go` (shared upstream error mapping), `services/gateway/main.go`, `api/openapi.yaml`, `scripts/smoke.sh`

**Interfaces:**
- Consumes: `identity.FromContext`, `identity.Role*` (task 1); `api.RequireRole`, `api.Authenticate` (task 1); `catalog.Restaurant`, `catalog.ErrInvalidArgument` (slice 1).
- Produces (tasks 3–5 rely on these):
  - **Schema.** `menu_items(id uuid PK, restaurant_id uuid NOT NULL REFERENCES restaurants, name text NOT NULL, price_minor bigint NOT NULL CHECK (price_minor > 0), available boolean NOT NULL DEFAULT true)`, index on `restaurant_id`. `orders(id uuid PK, customer_id uuid NOT NULL, restaurant_id uuid NOT NULL REFERENCES restaurants, status text NOT NULL CHECK (status IN (…the 8…)), total_minor bigint NOT NULL, currency text NOT NULL, delivery_lat double precision NOT NULL, delivery_lng double precision NOT NULL, delivery_address text NOT NULL DEFAULT '', version int NOT NULL DEFAULT 1, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now())`, indexes `(customer_id, created_at DESC)`, `(restaurant_id, created_at DESC)`, `(status)`. `order_items(order_id uuid REFERENCES orders ON DELETE CASCADE, line_no int, menu_item_id uuid NOT NULL, name text NOT NULL, unit_price_minor bigint NOT NULL, quantity int NOT NULL CHECK (quantity BETWEEN 1 AND 20), PRIMARY KEY (order_id, line_no))`. `idempotency_keys(scope text, key text, request_hash bytea NOT NULL, response jsonb, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (scope, key))`.
  - **Seed menu** (`00005`): 3 items per seeded restaurant, IDs `0193a1b2-0000-7000-8000-00000000RRII` (RR = restaurant 01–10, II = item 01–03), prices 9 000–32 000 minor; item `…000000000103` (Khreshchatyk Kitchen, item 3) is `available = false` for tests and demos.
  - **Proto** (add to `OrderService`; field numbers as written):
    ```proto
    rpc GetMenu(GetMenuRequest) returns (GetMenuResponse);       // NOT_FOUND: unknown restaurant
    rpc PlaceOrder(PlaceOrderRequest) returns (PlaceOrderResponse); // customer only; ALREADY_EXISTS: key reused
    rpc GetOrder(GetOrderRequest) returns (GetOrderResponse);     // NOT_FOUND: missing or not yours

    message GetMenuRequest { string restaurant_id = 1; }
    message MenuItem { string id = 1; string name = 2; int64 price_minor = 3; bool available = 4; }
    message GetMenuResponse { Restaurant restaurant = 1; repeated MenuItem items = 2; string currency = 3; }

    message PlaceOrderRequest { string idempotency_key = 1; string restaurant_id = 2; repeated OrderLine lines = 3; DeliveryLocation delivery = 4; }
    message OrderLine { string menu_item_id = 1; int32 quantity = 2; }
    message DeliveryLocation { double lat = 1; double lng = 2; string address = 3; }
    message PlaceOrderResponse { Order order = 1; bool replayed = 2; }

    message GetOrderRequest { string order_id = 1; }
    message GetOrderResponse { Order order = 1; }

    enum OrderStatus { ORDER_STATUS_UNSPECIFIED = 0; ORDER_STATUS_PENDING_PAYMENT = 1; ORDER_STATUS_AWAITING_RESTAURANT = 2; ORDER_STATUS_PREPARING = 3; ORDER_STATUS_READY = 4; ORDER_STATUS_PICKED_UP = 5; ORDER_STATUS_DELIVERED = 6; ORDER_STATUS_CANCELLED = 7; ORDER_STATUS_FAILED = 8; }
    message Order { string id = 1; string customer_id = 2; string restaurant_id = 3; OrderStatus status = 4; repeated OrderItem items = 5; int64 total_minor = 6; string currency = 7; DeliveryLocation delivery = 8; google.protobuf.Timestamp created_at = 9; int32 version = 10; }
    message OrderItem { string menu_item_id = 1; string name = 2; int64 unit_price_minor = 3; int32 quantity = 4; }
    ```
  - **catalog:** `type MenuItem struct { ID uuid.UUID; Name string; PriceMinor int64; Available bool }`; `(*Service).Menu(ctx, restaurantID uuid.UUID) (Restaurant, []MenuItem, error)` (`ErrNotFound` for an unknown restaurant; items ordered by ID); `Store` gains `Menu(ctx, uuid.UUID) (Restaurant, []MenuItem, error)`; `var ErrNotFound`.
  - **ordering** (`model.go`):
    - `type Status string` with the 8 constants `StatusPendingPayment = "PENDING_PAYMENT"` …; `ErrInvalidArgument`, `ErrNotFound`, `ErrKeyReused`, `ErrUnauthenticated`, `ErrPermissionDenied`.
    - `type Line struct { MenuItemID uuid.UUID; Quantity int }`; `type Delivery struct { Lat, Lng float64; Address string }`.
    - `type PlaceInput struct { IdempotencyKey string; RestaurantID uuid.UUID; Lines []Line; Delivery Delivery }`.
    - `type Item struct { MenuItemID uuid.UUID; Name string; UnitPriceMinor int64; Quantity int }`.
    - `type Order struct { ID, CustomerID, RestaurantID uuid.UUID; Status Status; Items []Item; TotalMinor int64; Currency string; Delivery Delivery; CreatedAt time.Time; Version int }`.
  - **ordering** (`service.go`):
    - `type Store interface { PlaceOrder(ctx context.Context, p Placement) (o Order, replayed bool, err error); GetOrder(ctx context.Context, id uuid.UUID) (Order, error) }`.
    - `type Placement struct { Scope, Key string; RequestHash []byte; NewID uuid.UUID; CustomerID uuid.UUID; Input PlaceInput; Now time.Time }` — everything the store's one transaction needs; pricing and the item/restaurant checks that need the DB run inside it (store returns `ErrInvalidArgument`-wrapped errors for those).
    - `func NewService(s Store, now func() time.Time) *Service`.
    - `(*Service).Place(ctx context.Context, caller identity.Identity, in PlaceInput) (Order, bool, error)`: role must be customer (`ErrPermissionDenied`); validates everything that needs no DB (key format, 1–20 lines, quantities, duplicates, coordinates); request hash = SHA-256 of the canonical JSON of `in` without the key.
    - `(*Service).Get(ctx context.Context, caller identity.Identity, id uuid.UUID) (Order, error)`: visibility rule from Global Constraints.
  - **ordering** (`store.go`): `func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore`. `PlaceOrder` in one `pgx` transaction (READ COMMITTED):
    1. `INSERT INTO idempotency_keys (scope, key, request_hash) VALUES (…) ON CONFLICT DO NOTHING RETURNING 1`.
    2. No row returned (a committed duplicate exists; a concurrent one has been waited for by Postgres): `SELECT request_hash, response …`; hash differs → `ErrKeyReused`; else decode `response` → `(order, replayed=true)`.
    3. Row returned: load restaurant location and the menu items (`WHERE id = ANY($1)`); run the DB-dependent checks (exists, same restaurant, available, ≤ 10 km); insert order + items with prices from the DB; `UPDATE idempotency_keys SET response = $order_json`; commit.
    Any error → rollback, so the key row disappears and a retry starts fresh (Review Focus 2).
  - **ordering** (`rpc.go`): `func NewHandler(svc *Service, log *slog.Logger) *Handler` with `PlaceOrder`, `GetOrder`. Identity from `identity.FromContext` (none → `UNAUTHENTICATED`). Errors: invalid → `INVALID_ARGUMENT`, not found → `NOT_FOUND`, key reused → `ALREADY_EXISTS`, unauthenticated → `UNAUTHENTICATED`, permission → `PERMISSION_DENIED`, ctx → `DEADLINE_EXCEEDED`/`CANCELED`, else `INTERNAL` (logged, generic message) — same policy as `catalog.toConnectError`.
  - **Order main.go:** `type orderServer struct { *catalog.Handler; *ordering.Handler }` passed to `orderv1connect.NewOrderServiceHandler`.
  - **Gateway HTTP contract** (all JSON fields camelCase; money fields `…Minor` + `currency`):
    - `GET /v1/restaurants/{id}/menu` (public) → `200 {"restaurant":{"id","name"},"currency":"UAH","items":[{"id","name","priceMinor","available"}]}`; malformed id → `400 invalid_argument`; unknown → `404 not_found`.
    - `POST /v1/orders` (`RequireRole(customer)`), header `Idempotency-Key` required, body `{"restaurantId","items":[{"menuItemId","quantity"}],"delivery":{"lat","lng","address"}}` (unknown fields rejected, ≤ 16 KiB) → `202` with the order body and `Location: /v1/orders/{id}`; a replay also sets `Idempotent-Replayed: true`. Missing/bad key or body → `400 invalid_argument`; `ALREADY_EXISTS` → `422 idempotency_key_reused`.
    - `GET /v1/orders/{id}` (`RequireRole()`) → `200` order body; `404 not_found`.
    - Order body: `{"id","customerId","restaurantId","status","items":[{"menuItemId","name","unitPriceMinor","quantity"}],"totalMinor","currency","delivery":{"lat","lng","address"},"createdAt","version"}`; `status` is the SPEC name (`"PENDING_PAYMENT"`).
    - Upstream error mapping (one function shared with `restaurants.go`): `INVALID_ARGUMENT` 400, `NOT_FOUND` 404, `ALREADY_EXISTS` 422, `UNAUTHENTICATED` 401, `PERMISSION_DENIED` 403, `UNAVAILABLE` 503, deadline 504, other 502.

- [ ] **Step 1: Schema, seed and queries.** Write the three migrations, `ordering.sql` (named queries for steps 1–3 above plus `GetOrder`/`GetOrderItems`), the menu query in `catalog.sql`, and the second sqlc package (`queries: db/queries/ordering.sql`, `out: internal/ordering/dbgen`; change the catalog entry's `queries` to `db/queries/catalog.sql`). Run `make gen` → generates without errors.
- [ ] **Step 2: Proto.** Add the RPCs and messages above. `make gen` → `buf lint` clean; `go tool -modfile=tools/go.mod buf breaking --against '.git#branch=main'` → no breaking changes (only additions).
- [ ] **Step 3: Failing `catalog` menu tests.** `service_test.go`: `TestMenu_UnknownRestaurantIsNotFound` (fake store returns `ErrNotFound` → passes through). `rpc_test.go`: `TestGetMenu_BadIDIsInvalidArgument` (`"nope"`), `TestGetMenu_NotFound`. `store_integration_test.go`: `TestStoreMenu_Seeded` (Khreshchatyk Kitchen → 3 items ordered by ID, item 3 `Available == false`, prices > 0). Run `go test -tags integration ./services/order/internal/catalog/...` → FAIL.
- [ ] **Step 4: Implement** the catalog menu (model, service, store, rpc). Run → PASS.
- [ ] **Step 5: Failing `ordering` service tests** (`service_test.go`, fake `Store` recording the `Placement`, fixed clock):
  - `TestPlace_RequiresCustomer`: courier caller → `ErrPermissionDenied`; store not called.
  - `TestPlace_Validation`, table, each `errors.Is(err, ErrInvalidArgument)` and store not called: empty key; key of 65 chars; key `"a b"`; no lines; 21 lines; quantity 0; quantity 21; same menu item twice; lat 91; lng NaN.
  - `TestPlace_HashIgnoresKeyButNotBody`: same input with two keys → same `RequestHash`; quantity changed → different hash.
  - `TestPlace_PassesCallerAndClock`: `Placement.Scope == caller.UserID`, `CustomerID` parsed from it, `Now == clock`, `NewID` is a version-7 UUID.
  - `TestGet_Visibility` *(Review Focus 5)*, table over a stored order of customer C at restaurant R: C → ok; another customer → `ErrNotFound`; staff of R → ok; staff of another restaurant → `ErrNotFound`; ops → ok; courier → `ErrNotFound`.
- [ ] **Step 6: Run** `go test ./services/order/internal/ordering/...` → FAIL. **Implement** `model.go`, `service.go`. Run → PASS.
- [ ] **Step 7: Failing `ordering` store integration tests** (`store_integration_test.go`, testcontainers Postgres with the real migrations, like `catalog`):
  - `TestPlaceOrder_PricesFromDatabase`: 2 × item `…0101` + 1 × `…0102` → total = 2·price₁ + price₂ read from `menu_items`; items carry the names and prices; status `PENDING_PAYMENT`; version 1.
  - `TestPlaceOrder_ConcurrentDuplicatesCreateOneOrder` *(Review Focus 1)*: 10 goroutines, same scope/key/hash → `SELECT count(*) FROM orders WHERE customer_id = …` is 1; all 10 returned orders are equal; exactly one has `replayed == false`.
  - `TestPlaceOrder_KeyReuseWithDifferentRequest` *(Review Focus 3)*: second call with another hash → `ErrKeyReused`; still one order.
  - `TestPlaceOrder_FailedAttemptDoesNotKeepKey` *(Review Focus 2)*: first call includes unavailable item `…0103` → `ErrInvalidArgument`; no key row left; second call, same key, valid lines → order created, `replayed == false`.
  - `TestPlaceOrder_KeysAreScopedPerCustomer` *(Review Focus 4)*: same key under two scopes → two different order IDs.
  - `TestPlaceOrder_RejectsForeignAndFarAway`: item from another restaurant → `ErrInvalidArgument`; delivery 11 km from the restaurant → `ErrInvalidArgument`; unknown restaurant → `ErrInvalidArgument`.
  - `TestGetOrder_RoundTrip`: get after place returns the same order; unknown ID → `ErrNotFound`.
- [ ] **Step 8: Run** `go test -tags integration ./services/order/internal/ordering/...` → FAIL. **Implement** `store.go`. Run → PASS.
- [ ] **Step 9: Failing `ordering` rpc tests** (`rpc_test.go`, fake store, in-process Connect server with `identity.Interceptor()`): no identity → `UNAUTHENTICATED`; courier → `PERMISSION_DENIED`; store `ErrKeyReused` → `ALREADY_EXISTS`; store `ErrNotFound` → `NOT_FOUND`; store `errors.New("db down")` → `INTERNAL` with message `"internal error"` (no `db down` in it); a placed order maps every field to the proto (status enum, timestamp, money). Run → FAIL. **Implement** `rpc.go`; wire `orderServer` in `main.go`. Run `go test ./services/order/...` → PASS.
- [ ] **Step 10: Failing Gateway tests** (`orders_test.go`, `menu_test.go`, fake `OrderServiceClient`):
  - Menu: OK mapping; id `"x"` → 400; upstream `NOT_FOUND` → 404 `not_found`.
  - Place: OK → 202, `Location`, body mapped, fake received the key from the header and lines/delivery from the body; replay → 202 + `Idempotent-Replayed: true`; no `Idempotency-Key` → 400; key `"a b"` → 400; unknown body field → 400; 17 KiB body → 400; upstream `ALREADY_EXISTS` → 422 `idempotency_key_reused`.
  - Get: OK; upstream `NOT_FOUND` → 404.
  - Routes (in `main_test.go`): `POST /v1/orders` without a token → 401; as a courier → 403 (Gateway check, Order not called); `GET /v1/restaurants/{id}/menu` without a token → reaches Order.
  Run → FAIL.
- [ ] **Step 11: Implement** `menu.go`, `orders.go`, the shared upstream error mapping and the new problem code; register routes in Gateway `main.go`. Run `make test` → PASS; `make lint` → clean.
- [ ] **Step 12: Contract and smoke.** `api/openapi.yaml`: the three paths, `Idempotency-Key` header parameter, the order/menu schemas, `idempotency_key_reused`. `scripts/smoke.sh`, new step after login: read Khreshchatyk Kitchen's menu; place an order as Olena with a fresh key → 202 `PENDING_PAYMENT`; **10 parallel identical POSTs with one new key → all 202 with the same order ID**; same key with another quantity → 422; `GET /v1/orders/{id}` as Olena → 200, as Taras → 404.
- [ ] **Step 13: Verify end to end.** `make clean && make up && make smoke` → passes. `make gen-check` → clean after committing generated code. `make test-int` → PASS.
- [ ] **Step 14: Stop for author review.** Commit after approval on branch `slice-2-task-2-orders`: `feat(order): menu, idempotent order placement and order lookup`. Push, PR, checks green.

---

## Carried to task 3 (agreed with the author, 2026-11-27)

- **Waiting duplicates and the 1s `statement_timeout`.** A concurrent duplicate waits on the `idempotency_keys` primary key inside `InsertIdempotencyKey`, and that wait counts toward its own 1s statement timeout (`platform/pg`). If the first transaction ever takes over 1s, waiting duplicates fail as INTERNAL → Gateway 502 instead of replaying. Correctness holds (still one order; the client can retry), but the answer is wrong. Task 3 adds the outbox write to this transaction, so its plan must decide: map Postgres `query_canceled` (SQLSTATE 57014) on the key insert to a retryable UNAVAILABLE/503, or give the key insert a longer per-statement timeout (`SET LOCAL statement_timeout`), and test it with a deliberately slow first transaction.
