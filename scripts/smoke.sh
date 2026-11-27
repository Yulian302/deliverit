#!/usr/bin/env bash
# End-to-end smoke test against the running stack (`make up`, then `make smoke`).
#
# It checks the whole walking skeleton, not just "the API answers":
#   1. the Gateway becomes ready;
#   2. a real search returns the 10 seeded restaurants, nearest first;
#   3. bad input is a 400 problem+json, not a 500;
#   4. demo login gives a token the Gateway accepts, and no token is a 401;
#   5. the search request is ONE trace in Tempo: gateway → order → postgres;
#   6. its log lines from both services are in Loki, linked by trace ID.
#
# Needs curl and jq. Exits non-zero, with the reason, on the first failure.
set -euo pipefail

GATEWAY=${GATEWAY_URL:-http://localhost:8080}
GRAFANA=${GRAFANA_URL:-http://localhost:3000}
TEMPO="$GRAFANA/api/datasources/proxy/uid/tempo"
LOKI="$GRAFANA/api/datasources/proxy/uid/loki"

pass() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
fail() { printf '  \033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }
step() { printf '\n%s\n' "$*"; }

# retry SECONDS CMD...: run CMD every second until it succeeds or time runs out.
retry() {
  local deadline=$((SECONDS + $1)); shift
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || return 1
    sleep 1
  done
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# ── 1 ──────────────────────────────────────────────────────────────────────
step "1. Gateway is ready"
retry 60 curl -fsS "$GATEWAY/readyz" || fail "$GATEWAY/readyz not 200 within 60s"
pass "GET /readyz → 200"

# ── 2 ──────────────────────────────────────────────────────────────────────
step "2. Search near Kyiv centre"
status=$(curl -sS -o "$tmp/body.json" -D "$tmp/headers" -w '%{http_code}' \
  "$GATEWAY/v1/restaurants?near=50.4501,30.5234")
[[ $status == 200 ]] || fail "status $status, want 200: $(cat "$tmp/body.json")"
count=$(jq '.restaurants | length' "$tmp/body.json")
[[ $count == 10 ]] || fail "$count restaurants, want the 10 seeded"
jq -e '[.restaurants[].distanceKm] | . == sort' "$tmp/body.json" >/dev/null || fail "results not nearest first"
header() { awk -v h="$1" -F': ' 'tolower($1) == tolower(h) { sub(/\r$/, "", $2); print $2 }' "$tmp/headers"; }
trace_id=$(header X-Trace-Id)
[[ $trace_id =~ ^[0-9a-f]{32}$ ]] || fail "no X-Trace-Id header (got '$trace_id')"
[[ -n $(header X-Request-Id) ]] || fail "no X-Request-Id header"
pass "200, 10 restaurants, nearest first (nearest: $(jq -r '.restaurants[0].name' "$tmp/body.json"))"
pass "trace $trace_id"

# ── 3 ──────────────────────────────────────────────────────────────────────
step "3. Bad input is a 400 problem, never a 500"
status=$(curl -sS -o "$tmp/bad.json" -D "$tmp/bad.headers" -w '%{http_code}' "$GATEWAY/v1/restaurants?near=abc")
[[ $status == 400 ]] || fail "near=abc: status $status, want 400"
grep -qi '^content-type: application/problem+json' "$tmp/bad.headers" || fail "near=abc: not application/problem+json"
[[ $(jq -r .code "$tmp/bad.json") == invalid_argument ]] || fail "near=abc: code $(jq -r .code "$tmp/bad.json"), want invalid_argument"
pass "near=abc → 400 invalid_argument (problem+json)"

# ── 4 ──────────────────────────────────────────────────────────────────────
step "4. Demo login and token check"
customer=0193a1b1-0000-7000-8000-0000000000c1 # Olena, a seeded demo customer
status=$(curl -sS -o "$tmp/login.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  -d "{\"userId\":\"$customer\"}" "$GATEWAY/v1/auth/demo-login")
[[ $status == 200 ]] || fail "demo-login: status $status, want 200: $(cat "$tmp/login.json")"
token=$(jq -r .token "$tmp/login.json")
[[ -n $token && $token != null ]] || fail "demo-login: no token in the response"
pass "POST /v1/auth/demo-login → 200, token for $(jq -r .user.name "$tmp/login.json")"
status=$(curl -sS -o "$tmp/me.json" -w '%{http_code}' -H "Authorization: Bearer $token" "$GATEWAY/v1/auth/me")
[[ $status == 200 ]] || fail "me with token: status $status, want 200"
[[ $(jq -r .role "$tmp/me.json") == customer ]] || fail "me: role $(jq -r .role "$tmp/me.json"), want customer"
pass "GET /v1/auth/me with the token → 200, role customer"
status=$(curl -sS -o "$tmp/anon.json" -w '%{http_code}' "$GATEWAY/v1/auth/me")
[[ $status == 401 ]] || fail "me without token: status $status, want 401"
[[ $(jq -r .code "$tmp/anon.json") == unauthenticated ]] || fail "me without token: code $(jq -r .code "$tmp/anon.json"), want unauthenticated"
pass "GET /v1/auth/me without a token → 401 unauthenticated"

# ── 5 ──────────────────────────────────────────────────────────────────────
step "5. One trace in Tempo: gateway → order → postgres"
# Spans are exported in batches every few seconds: poll until all are there.
# Each span as "service|kind|name" (first line of the name: SQL spans are
# named after the query).
spans_of_trace() {
  curl -fsS "$TEMPO/api/traces/$trace_id" | jq -r '
    .batches[]
    | (.resource.attributes[] | select(.key == "service.name") | .value.stringValue) as $svc
    | .scopeSpans[].spans[]
    | "\($svc)|\(.kind)|\(.name | split("\n")[0])"' > "$tmp/spans"
}
has_span() { grep -qxF "$1" "$tmp/spans"; }
trace_complete() {
  spans_of_trace &&
    has_span "gateway|SPAN_KIND_SERVER|GET /v1/restaurants" &&
    has_span "gateway|SPAN_KIND_CLIENT|deliverit.order.v1.OrderService/ListRestaurants" &&
    has_span "order|SPAN_KIND_SERVER|deliverit.order.v1.OrderService/ListRestaurants" &&
    has_span "order|SPAN_KIND_CLIENT|SELECT"
}
if ! retry 30 trace_complete; then
  echo "    spans found:" >&2; sed 's/^/      /' "$tmp/spans" >&2 2>/dev/null || true
  fail "trace $trace_id incomplete in Tempo after 30s"
fi
pass "gateway  SERVER  GET /v1/restaurants"
pass "gateway  CLIENT  OrderService/ListRestaurants"
pass "order    SERVER  OrderService/ListRestaurants"
pass "order    CLIENT  SELECT (postgres)"

# ── 6 ──────────────────────────────────────────────────────────────────────
step "6. Logs in Loki, linked to the trace"
logs_of_trace() {
  local now; now=$(date +%s)
  curl -fsS -G "$LOKI/loki/api/v1/query_range" \
    --data-urlencode "query={service_name=~\"gateway|order\"} | trace_id=\"$trace_id\"" \
    --data-urlencode "start=$((now - 600))000000000" \
    --data-urlencode "end=$((now + 60))000000000" \
    | jq -r '.data.result[].stream.service_name' | sort -u > "$tmp/log_services"
}
logs_complete() { logs_of_trace && grep -qx gateway "$tmp/log_services" && grep -qx order "$tmp/log_services"; }
retry 30 logs_complete || fail "no log lines for trace $trace_id from both services in Loki after 30s (found: $(tr '\n' ' ' < "$tmp/log_services" 2>/dev/null))"
pass "gateway and order log lines carry trace_id=$trace_id"

printf '\n\033[32mSmoke test passed.\033[0m Open the trace: %s/explore (Tempo, trace ID %s)\n' "$GRAFANA" "$trace_id"
