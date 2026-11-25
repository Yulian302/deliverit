package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

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

// setEnv sets the variables every Order test needs.
func setEnv(t *testing.T, addr, dsn string) {
	t.Helper()
	t.Setenv("SERVICE_NAME", "order")
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("DATABASE_URL", dsn)
}

// startOrder runs Order's serve mode in-process until the test ends or stop is
// called; done receives start's result.
func startOrder(t *testing.T, dsn string) (url string, stop context.CancelFunc, done <-chan error) {
	t.Helper()
	addr := freeAddr(t)
	setEnv(t, addr, dsn)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := make(chan error, 1)
	go func() { ch <- start(ctx, nil, io.Discard) }()

	url = "http://" + addr
	waitStatus(t, url+"/livez", http.StatusOK)
	return url, cancel, ch
}

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

// Review Focus 1: Postgres is down when Order starts. Order must stay up
// (alive), report not ready, and still shut down cleanly.
func TestOrder_StaysAliveButNotReadyWithoutDB(t *testing.T) {
	url, stop, done := startOrder(t, "postgres://u:p@127.0.0.1:1/db")

	waitStatus(t, url+"/readyz", http.StatusServiceUnavailable)

	stop()
	if err := waitReturn(t, done); err != nil {
		t.Fatalf("start returned %v after shutdown, want nil", err)
	}
}

// Connect-RPC clients (Gateway, Task 9) talk HTTP/2 without TLS ("h2c").
func TestOrder_SpeaksHTTP2Cleartext(t *testing.T) {
	url, _, _ := startOrder(t, "postgres://u:p@127.0.0.1:1/db")

	var protos http.Protocols
	protos.SetUnencryptedHTTP2(true) // HTTP/2 only, no TLS, no HTTP/1 fallback
	client := &http.Client{Transport: &http.Transport{Protocols: &protos}}

	resp, err := client.Get(url + "/livez")
	if err != nil {
		t.Fatalf("h2c request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Errorf("served over %s, want HTTP/2", resp.Proto)
	}
}

func TestOrder_UnknownModeFails(t *testing.T) {
	setEnv(t, freeAddr(t), "postgres://u:p@127.0.0.1:1/db")
	if err := start(context.Background(), []string{"frobnicate"}, io.Discard); err == nil {
		t.Fatal("start with an unknown mode returned nil")
	}
}

func TestOrder_MissingDatabaseURLFails(t *testing.T) {
	setEnv(t, freeAddr(t), "")
	if err := start(context.Background(), nil, io.Discard); err == nil {
		t.Fatal("start with no DATABASE_URL returned nil")
	}
}

// migrate mode gives up (instead of hanging forever) when the DB never
// comes up; the wait limit is shortened for the test.
func TestOrder_MigrateGivesUpWhenDBNeverComesUp(t *testing.T) {
	setEnv(t, freeAddr(t), "postgres://u:p@127.0.0.1:1/db")
	t.Setenv("MIGRATE_WAIT", "1s")

	began := time.Now()
	if err := start(context.Background(), []string{"migrate"}, io.Discard); err == nil {
		t.Fatal("migrate against an unreachable DB returned nil")
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("migrate took %v to give up, want about MIGRATE_WAIT (1s)", took)
	}
}

// `order healthcheck` is what Docker runs to decide if the container is
// healthy: it exits non-zero while /readyz is 503 (here: no database).
func TestOrder_HealthcheckModeReflectsReadiness(t *testing.T) {
	url, _, _ := startOrder(t, "postgres://u:p@127.0.0.1:1/db") // sets HTTP_ADDR for the probe too
	waitStatus(t, url+"/readyz", http.StatusServiceUnavailable)

	err := start(context.Background(), []string{"healthcheck"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("healthcheck while /readyz is 503 = %v, want the probe's status 503 error", err)
	}
}
