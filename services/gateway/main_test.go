package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
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

// startGateway runs the whole service in-process and returns its base URL,
// a stop function and a channel that receives start's result.
func startGateway(t *testing.T) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	addr := freeAddr(t)
	t.Setenv("SERVICE_NAME", "gateway")
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "") // no collector in unit tests

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
	url, stop, done := startGateway(t)

	waitStatus(t, url+"/readyz", http.StatusOK) // no dependencies to check yet

	stop() // what SIGTERM does
	if err := waitReturn(t, done); err != nil {
		t.Fatalf("start returned %v after shutdown, want nil", err)
	}
}

// Application routes go through the middleware chain; the request ID header
// on a plain 404 proves the chain is wired in front of the router.
func TestGateway_AppRoutesGoThroughMiddleware(t *testing.T) {
	url, _, _ := startGateway(t)

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
	t.Setenv("SERVICE_NAME", "") // required
	err := start(context.Background(), nil, io.Discard)
	if err == nil {
		t.Fatal("start with no SERVICE_NAME returned nil")
	}
}
