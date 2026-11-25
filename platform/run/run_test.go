package run_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Yulian302/deliverit/platform/health"
	"github.com/Yulian302/deliverit/platform/run"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type served struct {
	url  string
	hc   *health.Checker
	stop context.CancelFunc
	done chan error // receives Serve's return value
}

// startServer serves mux plus /readyz on a random local port.
func startServer(t *testing.T, mux *http.ServeMux, grace time.Duration) *served {
	t.Helper()
	hc := health.New()
	mux.Handle("/readyz", hc.ReadyHandler())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s := &served{url: "http://" + ln.Addr().String(), hc: hc, stop: cancel, done: make(chan error, 1)}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second} // plain server: these tests are about run, not httpx
	go func() { s.done <- run.Serve(ctx, discard, srv, ln, hc, grace) }()
	return s
}

// waitStarted fails the test instead of hanging if the handler never runs.
func waitStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
		return nil
	}
}

// Review Focus 4: SIGTERM while a request is in flight.
func TestServe_DrainsInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	})
	s := startServer(t, mux, 5*time.Second)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	type result struct {
		status int
		err    error
	}
	slow := make(chan result, 1)
	go func() {
		resp, err := client.Get(s.url + "/slow")
		if err != nil {
			slow <- result{err: err}
			return
		}
		_ = resp.Body.Close()
		slow <- result{status: resp.StatusCode}
	}()

	waitStarted(t, started)
	s.stop() // what SIGTERM does via SignalContext

	// While the slow request is still running, readiness must already fail.
	deadline := time.Now().Add(time.Second)
	for {
		rec := httptest.NewRecorder()
		s.hc.ReadyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz = %d during draining, want 503", rec.Code)
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case r := <-slow:
		t.Fatalf("in-flight request finished before /readyz turned 503 (draining too late): %+v", r)
	default:
	}

	if r := <-slow; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("in-flight request: status=%d err=%v, want 200", r.status, r.err)
	}
	if err := waitDone(t, s.done); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
	if _, err := client.Get(s.url + "/slow"); err == nil {
		t.Fatal("new request after shutdown succeeded, want connection error")
	}
}

func TestServe_GraceExceeded(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/stuck", func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release // stands in for a handler that takes far longer than the grace period
	})
	s := startServer(t, mux, 100*time.Millisecond)

	clientErr := make(chan error, 1)
	go func() {
		resp, err := http.Get(s.url + "/stuck")
		if err == nil {
			_ = resp.Body.Close()
		}
		clientErr <- err
	}()

	waitStarted(t, started)
	s.stop()

	err := waitDone(t, s.done)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Serve returned %v, want an error wrapping context.DeadlineExceeded", err)
	}
	// After the grace period the stuck connection is closed, not left hanging.
	select {
	case err := <-clientErr:
		if err == nil {
			t.Error("stuck request got a response, want a connection error")
		}
	case <-time.After(2 * time.Second):
		t.Error("stuck connection was not closed after the grace period ran out")
	}
}

func TestServeHTTP_ListenErrorIsReturned(t *testing.T) {
	srv := &http.Server{Addr: "127.0.0.1:-1", ReadHeaderTimeout: time.Second}
	err := run.ServeHTTP(context.Background(), discard, srv, health.New(), time.Second)
	if err == nil {
		t.Fatal("ServeHTTP with an invalid address returned nil")
	}
}
