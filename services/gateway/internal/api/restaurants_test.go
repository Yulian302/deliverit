package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
	"github.com/Yulian302/deliverit/platform/httpx"
	"github.com/Yulian302/deliverit/services/gateway/internal/api"
)

// fakeOrder is an OrderServiceClient that records the request and answers
// with resp or errFn's error. With block set, it waits for the request's
// context to end, like an Order that is too slow.
type fakeOrder struct {
	got   *orderv1.ListRestaurantsRequest
	resp  *orderv1.ListRestaurantsResponse
	errFn func(*orderv1.ListRestaurantsRequest) error
	block bool

	// The other RPCs (orders_test.go): canned responses, the error they
	// all return, and the last request each got.
	menu     *orderv1.GetMenuResponse
	placed   *orderv1.PlaceOrderResponse
	order    *orderv1.GetOrderResponse
	err      error
	gotMenu  *orderv1.GetMenuRequest
	gotPlace *orderv1.PlaceOrderRequest
	gotGet   *orderv1.GetOrderRequest
}

func (f *fakeOrder) ListRestaurants(ctx context.Context, req *orderv1.ListRestaurantsRequest) (*orderv1.ListRestaurantsResponse, error) {
	f.got = req
	if f.block {
		<-ctx.Done()
		return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
	}
	if f.errFn != nil {
		if err := f.errFn(req); err != nil {
			return nil, err
		}
	}
	if f.resp == nil {
		return &orderv1.ListRestaurantsResponse{}, nil
	}
	return f.resp, nil
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// get runs one GET through the handler (optionally wrapped) and returns the
// recorded response.
func get(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/restaurants"+query, nil))
	return rec
}

type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// wantProblem asserts an RFC 9457 problem response with the given status and code.
func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) problem {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %s)", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body)
	}
	if p.Code != code || p.Status != status || p.Type != "about:blank" || p.Title != http.StatusText(status) {
		t.Errorf("problem = %+v, want code=%s status=%d type=about:blank title=%q", p, code, status, http.StatusText(status))
	}
	return p
}

func TestRestaurants_OK(t *testing.T) {
	order := &fakeOrder{resp: &orderv1.ListRestaurantsResponse{Restaurants: []*orderv1.Restaurant{{
		Id: "0193a1b0-0000-7000-8000-000000000001", Name: "Khreshchatyk Kitchen",
		Lat: 50.448, Lng: 30.522, PrepMinutes: 15, DistanceKm: 0.25,
	}}}}

	rec := get(t, api.NewRestaurantsHandler(order, discard), "?near=50.45,30.52&radiusKm=3&limit=5")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if g := order.got; g.GetLat() != 50.45 || g.GetLng() != 30.52 || g.GetRadiusKm() != 3 || g.GetLimit() != 5 {
		t.Errorf("Order got %v, want lat=50.45 lng=30.52 radius=3 limit=5", g)
	}
	// Exact JSON field names are the public contract (api/openapi.yaml).
	var body struct {
		Restaurants []map[string]any `json:"restaurants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Restaurants) != 1 {
		t.Fatalf("got %d restaurants, want 1", len(body.Restaurants))
	}
	r := body.Restaurants[0]
	for field, want := range map[string]any{
		"id": "0193a1b0-0000-7000-8000-000000000001", "name": "Khreshchatyk Kitchen",
		"lat": 50.448, "lng": 30.522, "prepMinutes": 15.0, "distanceKm": 0.25,
	} {
		if r[field] != want {
			t.Errorf("%s = %v, want %v", field, r[field], want)
		}
	}
	if len(r) != 6 {
		t.Errorf("restaurant has fields %v, want exactly the 6 documented ones", r)
	}
}

func TestRestaurants_OptionalParamsAreOptional(t *testing.T) {
	order := &fakeOrder{}
	rec := get(t, api.NewRestaurantsHandler(order, discard), "?near=50.45,30.52")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if order.got.GetRadiusKm() != 0 || order.got.GetLimit() != 0 {
		t.Errorf("Order got %v, want radius and limit unset (0) so Order applies its defaults", order.got)
	}
}

func TestRestaurants_EmptyIsArray(t *testing.T) {
	rec := get(t, api.NewRestaurantsHandler(&fakeOrder{}, discard), "?near=0,0")

	if !strings.Contains(rec.Body.String(), `"restaurants":[]`) {
		t.Errorf("body = %s, want \"restaurants\":[] (never null)", rec.Body)
	}
}

// Review Focus 2: malformed input is always 400 problem+json, never a 500.
// The Gateway rejects what it can't parse; range rules (lat <= 90, limit >= 0)
// live only in Order, which answers InvalidArgument.
func TestRestaurants_BadParams(t *testing.T) {
	orderRules := func(req *orderv1.ListRestaurantsRequest) error {
		if req.GetLat() > 90 || req.GetLimit() < 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("out of range"))
		}
		return nil
	}
	for name, query := range map[string]string{
		"missing near":       "",
		"near not numbers":   "?near=abc",
		"near one number":    "?near=1",
		"near three numbers": "?near=1,2,3",
		"near lat 91":        "?near=91,0", // parses; Order rejects the range
		"limit -1":           "?near=1,2&limit=-1",
		"limit not int":      "?near=1,2&limit=2.5",
		"limit overflows":    "?near=1,2&limit=99999999999",
		"radius not number":  "?near=1,2&radiusKm=x",
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(t, api.NewRestaurantsHandler(&fakeOrder{errFn: orderRules}, discard), query)
			p := wantProblem(t, rec, http.StatusBadRequest, "invalid_argument")
			if p.Detail == "" {
				t.Error("400 without a detail: the caller can't tell what to fix")
			}
		})
	}
}

// Review Focus 3: Order is down. A real Connect client pointed at a port
// nothing listens on must turn into a quick 503, never a hang or a 500.
func TestRestaurants_UpstreamUnavailable(t *testing.T) {
	client := orderv1connect.NewOrderServiceClient(http.DefaultClient, "http://"+closedAddr(t))
	h := httpx.Deadline(5 * time.Second)(api.NewRestaurantsHandler(client, discard))

	start := time.Now()
	rec := get(t, h, "?near=50.45,30.52")

	wantProblem(t, rec, http.StatusServiceUnavailable, "upstream_unavailable")
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v, want a fast failure", took)
	}
}

// Connecting to Order timed out (a host that drops packets, cut short by the
// Gateway's dial timeout). Connect reports any network timeout as
// DeadlineExceeded, but this is "couldn't reach Order", not "Order was too
// slow": it must be 503, not 504.
func TestRestaurants_ConnectTimeoutIsUnavailable(t *testing.T) {
	// A 1ns dial timeout fails at once with Go's real dial-timeout error,
	// the same one a black-holed host produces after the Gateway's 1s.
	dialTimesOut := &http.Client{Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: time.Nanosecond}).DialContext,
	}}
	client := orderv1connect.NewOrderServiceClient(dialTimesOut, "http://"+closedAddr(t))
	h := httpx.Deadline(5 * time.Second)(api.NewRestaurantsHandler(client, discard))

	wantProblem(t, get(t, h, "?near=50.45,30.52"), http.StatusServiceUnavailable, "upstream_unavailable")
}

// Review Focus 3: Order is too slow. The request deadline (Deadline
// middleware, 5s in production) ends the wait with a 504.
func TestRestaurants_UpstreamTimeout(t *testing.T) {
	h := httpx.Deadline(100 * time.Millisecond)(api.NewRestaurantsHandler(&fakeOrder{block: true}, discard))

	start := time.Now()
	rec := get(t, h, "?near=50.45,30.52")

	wantProblem(t, rec, http.StatusGatewayTimeout, "upstream_timeout")
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %v, want ~100ms", took)
	}
}

// Any other upstream failure is a 502. Order's message stays in our logs:
// it may describe internals the public must not see.
func TestRestaurants_OtherErrorIs502(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	order := &fakeOrder{errFn: func(*orderv1.ListRestaurantsRequest) error {
		return connect.NewError(connect.CodeInternal, errors.New("pq: relation \"secret\" does not exist"))
	}}

	rec := get(t, api.NewRestaurantsHandler(order, log), "?near=50.45,30.52")

	wantProblem(t, rec, http.StatusBadGateway, "upstream_error")
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("upstream error leaked to the client: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), "secret") {
		t.Errorf("upstream error not logged: %q", logs.String())
	}
}

// The Gateway's deadline must travel to Order (Connect sends the remaining
// time in a header), so Order also stops working when the Gateway gives up.
func TestRestaurants_DeadlineReachesOrder(t *testing.T) {
	var orderDeadline time.Time
	var hasDeadline bool
	mux := http.NewServeMux()
	mux.Handle(orderv1connect.NewOrderServiceHandler(deadlineRecorder{record: func(ctx context.Context) {
		orderDeadline, hasDeadline = ctx.Deadline()
	}}))
	order := httptest.NewServer(mux)
	t.Cleanup(order.Close)

	client := orderv1connect.NewOrderServiceClient(order.Client(), order.URL)
	h := httpx.Deadline(2 * time.Second)(api.NewRestaurantsHandler(client, discard))
	rec := get(t, h, "?near=50.45,30.52")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if !hasDeadline {
		t.Fatal("Order's handler had no deadline: the Gateway's deadline was not propagated")
	}
	if left := time.Until(orderDeadline); left <= 0 || left > 2*time.Second {
		t.Errorf("Order's deadline is %v away, want within the Gateway's 2s", left)
	}
}

// closedAddr returns a local address that refuses connections right away:
// a port we just listened on and closed. (A fixed port like 127.0.0.1:1 is
// not reliable: some sandboxes and firewalls silently drop packets to it, so
// the dial hangs instead of being refused.)
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// deadlineRecorder is an Order server that records each ListRestaurants
// call's context; the other RPCs are unimplemented.
type deadlineRecorder struct {
	orderv1connect.UnimplementedOrderServiceHandler
	record func(context.Context)
}

func (d deadlineRecorder) ListRestaurants(ctx context.Context, _ *orderv1.ListRestaurantsRequest) (*orderv1.ListRestaurantsResponse, error) {
	d.record(ctx)
	return &orderv1.ListRestaurantsResponse{}, nil
}
