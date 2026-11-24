package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
)

// freeAddr returns a local address nothing listens on right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// startGateway runs the whole service in-process, talking to Order at
// orderURL, and returns its base URL, a stop function and a channel that
// receives start's result.
func startGateway(t *testing.T, orderURL string) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	addr := freeAddr(t)
	t.Setenv("SERVICE_NAME", "gateway")
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "") // no collector in unit tests
	t.Setenv("ORDER_URL", orderURL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- start(ctx, nil, io.Discard) }()

	url := "http://" + addr
	waitStatus(t, url+"/livez", http.StatusOK)
	return url, cancel, done
}

// waitStatus polls url until it answers with want, failing after 10s.
func waitStatus(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := 0
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			last = resp.StatusCode
			if last == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s: last status %d, want %d", url, last, want)
}

func waitReturn(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("start did not return after cancel")
		return nil
	}
}

func TestGateway_ServesHealthAndShutsDownCleanly(t *testing.T) {
	url, stop, done := startGateway(t, "http://"+freeAddr(t))

	waitStatus(t, url+"/readyz", http.StatusOK) // no dependencies to check yet

	stop() // what SIGTERM does
	if err := waitReturn(t, done); err != nil {
		t.Fatalf("start returned %v after shutdown, want nil", err)
	}
}

// Application routes go through the middleware chain; the request ID header
// on a plain 404 proves the chain is wired in front of the router.
func TestGateway_AppRoutesGoThroughMiddleware(t *testing.T) {
	url, _, _ := startGateway(t, "http://"+freeAddr(t))

	resp, err := http.Get(url + "/no-such-route")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("no X-Request-ID on an app route: middleware chain not applied")
	}
}

func TestGateway_InvalidConfigFails(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no SERVICE_NAME": {"SERVICE_NAME": "", "ORDER_URL": "http://order:8081"},
		"no ORDER_URL":    {"SERVICE_NAME": "gateway", "ORDER_URL": ""},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			t.Setenv("HTTP_ADDR", freeAddr(t))
			// If the config were wrongly accepted, start would serve until
			// ctx ends; the deadline turns that into a failure, not a hang.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := start(ctx, nil, io.Discard); err == nil {
				t.Fatal("start accepted the config (served until the deadline), want a config error")
			}
		})
	}
}

// fakeOrderServer is an Order stand-in speaking HTTP/2 without TLS (h2c),
// like the real one. It records the HTTP protocol each call arrived on.
func fakeOrderServer(t *testing.T) (url string, protoMajor *atomic.Int32) {
	t.Helper()
	protoMajor = new(atomic.Int32)
	mux := http.NewServeMux()
	mux.Handle(orderv1connect.NewOrderServiceHandler(fakeOrder{}))
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protoMajor.Store(int32(r.ProtoMajor))
			mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: time.Second,
		Protocols:         new(http.Protocols),
	}
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String(), protoMajor
}

type fakeOrder struct{}

func (fakeOrder) ListRestaurants(context.Context, *orderv1.ListRestaurantsRequest) (*orderv1.ListRestaurantsResponse, error) {
	return &orderv1.ListRestaurantsResponse{Restaurants: []*orderv1.Restaurant{{Id: "r-1", Name: "Fake Pho"}}}, nil
}

// The whole Gateway binary: GET /v1/restaurants goes through the middleware
// to Order, over HTTP/2 cleartext, and back as public JSON.
func TestGateway_RestaurantsReachOrder(t *testing.T) {
	orderURL, protoMajor := fakeOrderServer(t)
	url, _, _ := startGateway(t, orderURL)

	resp, err := http.Get(url + "/v1/restaurants?near=50.45,30.52")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"Fake Pho"`) {
		t.Fatalf("status %d, body %s; want 200 with the fake restaurant", resp.StatusCode, body)
	}
	if got := protoMajor.Load(); got != 2 {
		t.Errorf("Gateway called Order over HTTP/%d, want HTTP/2 (h2c)", got)
	}
}

func TestGateway_RestaurantsOnlyAllowsGET(t *testing.T) {
	url, _, _ := startGateway(t, "http://"+freeAddr(t))

	resp, err := http.Post(url+"/v1/restaurants?near=1,2", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", resp.StatusCode)
	}
}

// Review Focus 3 through the real binary: Order down → fast 503 problem.
func TestGateway_OrderDownIs503(t *testing.T) {
	url, _, _ := startGateway(t, "http://"+freeAddr(t)) // nothing listens there

	start := time.Now()
	resp, err := http.Get(url + "/v1/restaurants?near=50.45,30.52")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v, want a fast failure", took)
	}
}
