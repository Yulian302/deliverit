package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/Yulian302/deliverit/platform/health"
)

type readyBody struct {
	Status string   `json:"status"`
	Failed []string `json:"failed"`
}

// get calls a handler and decodes the JSON body.
func get(t *testing.T, h http.Handler) (int, readyBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body readyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func pass(context.Context) error { return nil }
func fail(context.Context) error { return errors.New("down") }

func TestLive_AlwaysOK(t *testing.T) {
	c := health.New()
	c.Add("db", fail) // liveness ignores dependencies

	code, body := get(t, c.LiveHandler())

	if code != http.StatusOK || body.Status != "ok" {
		t.Fatalf("got %d %q, want 200 ok", code, body.Status)
	}
}

func TestReady_AllChecksPass(t *testing.T) {
	c := health.New()
	c.Add("db", pass)
	c.Add("cache", pass)

	code, body := get(t, c.ReadyHandler())

	if code != http.StatusOK || body.Status != "ok" {
		t.Fatalf("got %d %q, want 200 ok", code, body.Status)
	}
}

func TestReady_FailingCheckListed(t *testing.T) {
	c := health.New()
	c.Add("db", fail)
	c.Add("cache", pass)

	code, body := get(t, c.ReadyHandler())

	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", code)
	}
	if body.Status != "unavailable" {
		t.Fatalf("status = %q, want unavailable", body.Status)
	}
	if !slices.Equal(body.Failed, []string{"db"}) {
		t.Fatalf("failed = %v, want [db]", body.Failed)
	}
}

func TestReady_Draining(t *testing.T) {
	c := health.New()
	c.Add("db", pass)
	c.SetDraining()

	code, body := get(t, c.ReadyHandler())
	if code != http.StatusServiceUnavailable || body.Status != "draining" {
		t.Fatalf("ready: got %d %q, want 503 draining", code, body.Status)
	}

	code, body = get(t, c.LiveHandler())
	if code != http.StatusOK || body.Status != "ok" {
		t.Fatalf("live while draining: got %d %q, want 200 ok", code, body.Status)
	}
}

func TestReady_SlowCheckTimesOut(t *testing.T) {
	c := health.New()
	c.Add("slow", func(ctx context.Context) error {
		<-ctx.Done() // never succeeds; only the timeout can end it
		return ctx.Err()
	})

	start := time.Now()
	code, body := get(t, c.ReadyHandler())
	elapsed := time.Since(start)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", code)
	}
	if !slices.Equal(body.Failed, []string{"slow"}) {
		t.Fatalf("failed = %v, want [slow]", body.Failed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("took %v, want <= 1.5s (checks must time out after 1s)", elapsed)
	}
}
