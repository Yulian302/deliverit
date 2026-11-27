# Slice 2, Task 1 — Demo Auth Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A client logs in as a seeded demo user at `POST /v1/auth/demo-login`, gets an EdDSA JWT, and the Gateway verifies it on later requests and passes the caller's identity to internal services in trusted RPC headers.

**Architecture:** The Gateway alone issues and verifies tokens (`services/gateway/internal/auth`: keys, demo users, issuer). An HTTP middleware puts the verified identity in the request context; a shared Connect interceptor (`platform/identity`) copies it into RPC headers on the client side and back into the context on the server side. Internal services never see the JWT or the key (author's decision, 2026-11-27).

**Tech Stack:** Go 1.26, `github.com/golang-jwt/jwt/v5` (EdDSA), `crypto/ed25519`, Connect-RPC interceptors, existing `platform/*`.

**Spec:** `docs/SPEC.md` §9.6 (JWT EdDSA, roles, ownership), §10 (`POST /v1/auth/demo-login {userId} → {token}`), §6 (Gateway owns demo identities). Slice 2 order agreed with the author: auth → orders + idempotency → outbox + Kafka → payment → cancel + proof. Only this task is planned here; each later task gets its own plan before it starts.

## Global Constraints

- Everything in the slice 1 plan's Global Constraints still applies (module path, `internal/` boundaries, sqlc-only SQL, 5s request deadline, problem+json errors with stable `code`, env-only config, pinned tools).
- **Commit messages carry no `Co-Authored-By` trailer** (author's instruction).
- JWT: algorithm **EdDSA (Ed25519) only**; issuer `deliverit-gateway`; audience `deliverit`; claims `sub`, `role`, `rid` (restaurant users only), `iat`, `exp`, `jti`. Verification allows **30s** clock leeway.
- Roles, exactly: `customer`, `restaurant`, `courier`, `ops`.
- Config: `JWT_SIGNING_KEY` = standard base64 of a 32-byte Ed25519 seed, required; `JWT_TTL` default `12h`. The committed dev key is labelled **dev only** wherever it appears.
- Identity headers (Gateway → services): `Deliverit-User-Id`, `Deliverit-User-Role`, `Deliverit-Restaurant-Id`. They are trusted because internal RPC is on the private network (SPEC §9.6; mTLS is the upgrade). Known gap, accepted: compose publishes Order's port 8081 to the host for debugging, so anything on the host can set them.
- Error codes added to the public contract: `unauthenticated` (401, with `WWW-Authenticate: Bearer realm="deliverit"`), `forbidden` (403), `not_found` (404). Problem `detail` texts are fixed strings; never echo a token or header value.

## Review Focus

1. **Forged tokens:** `alg: none`, HS256 signed with the public key as an HMAC secret, a token from another key, a tampered payload → all 401, never accepted. → `TestVerify_Rejects`.
2. **Malformed `Authorization` headers** (`Basic …`, `Bearer` with no token, garbage, lowercase `bearer` which is valid) → 401 problem+json or success, never 500; the token never appears in the response or the logs. → `TestAuthenticate`.
3. **Identity headers sent from the internet:** a public request carrying `Deliverit-User-Id: <someone>` must not reach Order as an identity. → `TestGateway_DoesNotForwardIdentityHeaders`.
4. **Clock skew and expiry:** a token expired 10s ago still works (leeway), one expired 31s ago is 401. → `TestVerify_Rejects`, `TestVerify_LeewayAcceptsSmallSkew`.
5. **Bad config:** missing, non-base64 or wrong-length `JWT_SIGNING_KEY` stops startup with an error naming `JWT_SIGNING_KEY`, never a panic or a running Gateway that can't sign. → `TestParseSigningKey`, `TestGateway_InvalidConfigFails`.

---

### Task 1: Demo login, JWT verification and identity propagation

**Files:**
- Create: `platform/identity/identity.go`, `platform/identity/identity_test.go`
- Create: `services/gateway/internal/auth/keys.go`, `users.go`, `token.go`, `auth_test.go`
- Create: `services/gateway/internal/api/auth.go` (middleware), `login.go`, `me.go`, `auth_test.go`
- Modify: `services/gateway/internal/api/problem.go` (new codes), `services/gateway/main.go` (config, routes, middleware, `keygen`, client interceptor), `services/gateway/main_test.go`, `services/gateway/.env.example`
- Modify: `services/order/main.go` (server interceptor)
- Modify: `deploy/compose/docker-compose.yml` (gateway env), `api/openapi.yaml`, `scripts/smoke.sh`, `README.md` (login example), `go.mod`/`go.sum`

**Interfaces:**
- Consumes: `api.WriteProblem`, `httpx.Chain`, `config.Load`, `telemetry.Route` (slice 1).
- Produces (later tasks rely on these):
  - `platform/identity`:
    - `type Role string`; `RoleCustomer`, `RoleRestaurant`, `RoleCourier`, `RoleOps`; `func ParseRole(s string) (Role, error)`.
    - `type Identity struct { UserID string; Role Role; RestaurantID string }`.
    - `func NewContext(ctx context.Context, id Identity) context.Context`; `func FromContext(ctx context.Context) (Identity, bool)`.
    - `HeaderUserID`, `HeaderRole`, `HeaderRestaurantID` constants (values above).
    - `func Interceptor() connect.Interceptor`: client side writes the headers from ctx (none if ctx has no identity); server side reads them into ctx. No user ID header → no identity (public procedures still work). User ID present but role invalid → `connect.CodeUnauthenticated`.
  - `services/gateway/internal/auth`:
    - `func ParseSigningKey(b64 string) (ed25519.PrivateKey, error)`; `func GenerateSigningKey() (string, error)`.
    - `type User struct { ID, Name string; Role identity.Role; RestaurantID string }`; `func DemoUsers() []User`; `func FindUser(id string) (User, bool)`.
    - `var ErrInvalidToken = errors.New("invalid token")`.
    - `func NewIssuer(key ed25519.PrivateKey, ttl time.Duration, now func() time.Time) *Issuer`; `(*Issuer).Issue(u User) (token string, expiresAt time.Time, err error)`; `(*Issuer).Verify(token string) (identity.Identity, error)` (every failure wraps `ErrInvalidToken`).
  - `services/gateway/internal/api`:
    - `func Authenticate(iss *auth.Issuer) func(http.Handler) http.Handler`: no `Authorization` header → passes through with no identity; `Bearer <token>` (scheme case-insensitive) and valid → identity in ctx, span attributes `enduser.id`, `enduser.role`; anything else → 401.
    - `func RequireRole(roles ...identity.Role) func(http.Handler) http.Handler`: no identity → 401; role not listed → 403; no roles listed = any signed-in user.
    - `func NewDemoLoginHandler(iss *auth.Issuer) http.Handler`, `func NewMeHandler() http.Handler`.
    - `CodeUnauthenticated = "unauthenticated"`, `CodeForbidden = "forbidden"`, `CodeNotFound = "not_found"`.
  - HTTP contract:
    - `POST /v1/auth/demo-login`, body `{"userId":"<uuid>"}` (max 4 KiB, unknown fields rejected) → `200 {"token","tokenType":"Bearer","expiresAt":"<RFC 3339>","user":{"id","name","role","restaurantId"?}}`; bad body → `400 invalid_argument`; unknown user → `404 not_found`.
    - `GET /v1/auth/me` (any signed-in user) → `200 {"id","role","restaurantId"?}`.
    - `GET /v1/restaurants` stays public.
  - Demo users (fixed IDs, valid UUIDs; restaurant users point at seeded restaurants):

    | ID | Name | Role | Restaurant |
    |---|---|---|---|
    | `0193a1b1-0000-7000-8000-0000000000c1` | Olena | customer | |
    | `0193a1b1-0000-7000-8000-0000000000c2` | Taras | customer | |
    | `0193a1b1-0000-7000-8000-0000000000a1` | Khreshchatyk Kitchen staff | restaurant | `0193a1b0-0000-7000-8000-000000000001` |
    | `0193a1b1-0000-7000-8000-0000000000a5` | Podil Pierogi staff | restaurant | `0193a1b0-0000-7000-8000-000000000005` |
    | `0193a1b1-0000-7000-8000-0000000000b1` | Courier Andrii | courier | |
    | `0193a1b1-0000-7000-8000-0000000000b2` | Courier Iryna | courier | |
    | `0193a1b1-0000-7000-8000-0000000000d1` | Ops desk | ops | |
  - Gateway: settings gain `JWTSigningKey string \`env:"JWT_SIGNING_KEY,required,notEmpty"\`` and `JWTTTL time.Duration \`env:"JWT_TTL" envDefault:"12h"\``; the key is parsed before serving. New mode `gateway keygen` prints a fresh key and needs no other config. Middleware order: `RequestID, AccessLog, Recover, Authenticate, Deadline`. Order client interceptors: `tracing, identity.Interceptor()`.
  - Order: server interceptors `tracing, identity.Interceptor(), rpcx.AccessLog(log)`.

- [ ] **Step 1: Write the failing `platform/identity` tests.** Use `connect.NewUnaryHandler`/`connect.NewClient` with `emptypb.Empty` on an `httptest` server (no generated service needed).
  - `TestParseRole`: the four roles parse; `""`, `"admin"`, `"Customer"` → error.
  - `TestInterceptor_RoundTrip`: client ctx has `{UserID: "u1", Role: RoleRestaurant, RestaurantID: "r1"}` → handler's `FromContext` returns the same value and `ok == true`.
  - `TestInterceptor_NoIdentity`: no identity on the client → handler's `FromContext` gives `ok == false`, call succeeds.
  - `TestInterceptor_BadRoleIsUnauthenticated`: a raw client sets `Deliverit-User-Id: u1`, `Deliverit-User-Role: admin` → `connect.CodeOf(err) == connect.CodeUnauthenticated`.
- [ ] **Step 2: Run** `go test ./platform/identity/...` → FAIL (package missing).
- [ ] **Step 3: Implement `platform/identity/identity.go`** per Interfaces (unary + streaming handler + streaming client).
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Write the failing `auth` tests** in `services/gateway/internal/auth/auth_test.go` (fixed clock `now := func() time.Time { return t0 }`, TTL 1h):
  - `TestParseSigningKey` *(Review Focus 5)*: a 32-byte seed in base64 → key; 16 bytes, `"%%%"`, `""` → error whose text contains `JWT_SIGNING_KEY`.
  - `TestGenerateSigningKey`: output parses with `ParseSigningKey`; two calls differ.
  - `TestIssueVerify_RoundTrip`: issue for the Khreshchatyk staff user → `Verify` gives `{UserID: …a1, Role: restaurant, RestaurantID: …0001}`; `expiresAt == t0.Add(time.Hour)`.
  - `TestVerify_Rejects` *(Review Focus 1, 4)*, table, each `errors.Is(err, ErrInvalidToken)`: expired 31s ago; signed by another Ed25519 key; `alg: none`; HS256 signed with the public key bytes as secret; issuer `other`; audience `other`; no `exp`; payload segment altered after signing; `"not.a.jwt"`; role `admin`; role `restaurant` without `rid`.
  - `TestVerify_LeewayAcceptsSmallSkew` *(Review Focus 4)*: verify at `t0.Add(time.Hour + 10*time.Second)` → OK.
  - `TestDemoUsers`: IDs unique and valid UUIDs; every role appears; restaurant users have a `RestaurantID` from the seed (`…0001`, `…0005`); `FindUser` finds each and misses an unknown ID.
- [ ] **Step 6: Run** `go test ./services/gateway/internal/auth/...` → FAIL.
- [ ] **Step 7: Add the dependency and implement** `keys.go`, `users.go`, `token.go`. `go get github.com/golang-jwt/jwt/v5@<latest release at least two weeks old>`. Verify with `jwt.WithValidMethods([]string{"EdDSA"})`, `WithIssuer`, `WithAudience`, `WithExpirationRequired`, `WithLeeway(30*time.Second)`, `WithTimeFunc(now)`; `jti` is a UUIDv7 (`github.com/google/uuid`, already a dependency).
- [ ] **Step 8: Run** → PASS.
- [ ] **Step 9: Write the failing `api` tests** in `services/gateway/internal/api/auth_test.go`:
  - `TestDemoLogin_OK`: `{"userId":"…c1"}` → 200; `tokenType == "Bearer"`; the token verifies to the c1 identity; `user.role == "customer"`; no `restaurantId` key in `user`.
  - `TestDemoLogin_BadRequests`, table → `400 invalid_argument`: `not json`; empty body; `{}`; `{"userId":""}`; `{"userId":"…c1","extra":1}`; a 5 KiB body. Unknown `userId` → `404 not_found`.
  - `TestAuthenticate` *(Review Focus 2)*, table over the `Authorization` header: none → next runs, no identity; `Bearer <valid>` and `bearer <valid>` → next runs with identity; `Basic dXNlcjpwYXNz`, `Bearer`, `Bearer ` + expired token, `Bearer garbage` → 401, `code == "unauthenticated"`, `WWW-Authenticate: Bearer realm="deliverit"`, and the response body does not contain the sent token string.
  - `TestRequireRole`: no identity → 401; customer on an `ops`-only route → `403 forbidden`; ops → 200; `RequireRole()` with no roles lets a courier through.
  - `TestMe`: signed in as a restaurant user → `{"id":"…a1","role":"restaurant","restaurantId":"…0001"}`.
- [ ] **Step 10: Run** `go test ./services/gateway/internal/api/...` → FAIL.
- [ ] **Step 11: Implement** `api/auth.go`, `login.go`, `me.go` and the new codes in `problem.go`.
- [ ] **Step 12: Run** → PASS.
- [ ] **Step 13: Write the failing Gateway wiring tests** in `services/gateway/main_test.go`. `startGateway` sets `JWT_SIGNING_KEY` to a test key.
  - `TestGateway_LoginThenMe`: log in as c1 → `GET /v1/auth/me` with the token → 200, `role == "customer"`; without the token → 401.
  - `TestGateway_IdentityReachesOrder`: the fake Order server is wrapped with `identity.Interceptor()` and records `FromContext`; `GET /v1/restaurants` with c1's token → Order saw UserID c1, role customer.
  - `TestGateway_DoesNotForwardIdentityHeaders` *(Review Focus 3)*: `GET /v1/restaurants` with no token and `Deliverit-User-Id: …d1`, `Deliverit-User-Role: ops` → Order saw no identity.
  - Extend `TestGateway_InvalidConfigFails` *(Review Focus 5)*: missing `JWT_SIGNING_KEY` and `JWT_SIGNING_KEY=short` → `start` returns an error containing `JWT_SIGNING_KEY`.
  - `TestGateway_KeygenMode`: `start(ctx, []string{"keygen"}, &buf)` with no env set → nil error; `buf` holds one line that `auth.ParseSigningKey` accepts.
- [ ] **Step 14: Run** `go test ./services/gateway/...` → FAIL.
- [ ] **Step 15: Wire it up:** Gateway `main.go` (settings, `keygen`, routes `POST /v1/auth/demo-login` and `GET /v1/auth/me` wrapped in `RequireRole()`, `Authenticate` in the chain, `identity.Interceptor()` on the Order client); Order `main.go` (server interceptor).
- [ ] **Step 16: Run** `make test` → PASS; `make lint` → clean.
- [ ] **Step 17: Config, contract and smoke.**
  - Generate the dev key once with `go run ./services/gateway keygen`; put it in `services/gateway/.env.example` and the compose `gateway` environment, each with a `# DEV ONLY — never use outside local development` comment. Add `JWT_TTL=12h` to `.env.example`. Your existing `services/gateway/.env` is git-ignored and won't get it: add the two lines by hand (or delete the file so `make run-gateway` recreates it).
  - `api/openapi.yaml`: the two paths, a `bearerAuth` security scheme (HTTP bearer, JWT), the new problem codes.
  - `scripts/smoke.sh`, new step after the search: log in as c1 → token; `GET /v1/auth/me` with it → 200 and `role == customer`; without it → 401, `code == unauthenticated`. Renumber the later steps.
  - `README.md`: under "Try the API", the login + `/v1/auth/me` curl pair and a one-line pointer to the demo user table in `services/gateway/internal/auth/users.go`.
- [ ] **Step 18: Verify end to end.** `make clean && make up && make smoke` → passes, including the new auth step. `make gen-check` → clean (no proto changes in this task).
- [ ] **Step 19: Stop for author review.** Commit after approval on branch `slice-2-task-1-auth`: `feat(auth): demo login, JWT verification and identity propagation`. Push, open the PR, checks green.
