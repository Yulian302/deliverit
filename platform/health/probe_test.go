package health_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Yulian302/deliverit/platform/health"
)

// serveReadyz starts a server whose /readyz answers status, and returns the
// HTTP_ADDR-style address a service would be configured with (":<port>").
func serveReadyz(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	return ":" + port
}

func TestProbeLocal_ReadyIsNil(t *testing.T) {
	if err := health.ProbeLocal(context.Background(), serveReadyz(t, http.StatusOK)); err != nil {
		t.Fatalf("ProbeLocal on a ready service = %v, want nil", err)
	}
}

func TestProbeLocal_NotReadyIsError(t *testing.T) {
	if err := health.ProbeLocal(context.Background(), serveReadyz(t, http.StatusServiceUnavailable)); err == nil {
		t.Fatal("ProbeLocal on a 503 /readyz = nil, want an error")
	}
}

func TestProbeLocal_NothingListeningIsError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()

	if err := health.ProbeLocal(context.Background(), ":"+port); err == nil {
		t.Fatal("ProbeLocal with nothing listening = nil, want an error")
	}
}

// A hung service must fail the probe quickly, not hang the health checker.
func TestProbeLocal_HungServiceTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	start := time.Now()
	err := health.ProbeLocal(context.Background(), ":"+port)
	if err == nil {
		t.Fatal("ProbeLocal on a hung service = nil, want an error")
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("took %v, want it bounded by the probe timeout", took)
	}
}

// Services listen on all interfaces (":8081", "0.0.0.0:8081", "[::]:8081");
// the probe must always call the loopback address inside the container.
func TestLocalReadyURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8081":          "http://127.0.0.1:8081/readyz",
		"0.0.0.0:8081":   "http://127.0.0.1:8081/readyz",
		"[::]:8081":      "http://127.0.0.1:8081/readyz",
		"127.0.0.1:9000": "http://127.0.0.1:9000/readyz",
	} {
		got, err := health.LocalReadyURL(addr)
		if err != nil || got != want {
			t.Errorf("LocalReadyURL(%q) = %q, %v; want %q", addr, got, err, want)
		}
	}
	if _, err := health.LocalReadyURL("no-port"); err == nil {
		t.Error("LocalReadyURL without a port = nil error, want an error")
	}
}
