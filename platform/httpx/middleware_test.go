package httpx_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Yulian302/deliverit/platform/httpx"
	"github.com/Yulian302/deliverit/platform/logging"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// serveRequestID runs one request with the given X-Request-ID (none if
// empty) through RequestID and returns the response header and the ID the
// handler saw in its context.
func serveRequestID(t *testing.T, incoming string) (header, inCtx string) {
	t.Helper()
	h := httpx.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		inCtx = logging.RequestIDFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if incoming != "" {
		// Set the map directly (canonical key) so junk like "\n" isn't touched on the way in.
		req.Header[http.CanonicalHeaderKey(httpx.HeaderRequestID)] = []string{incoming}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Header().Get(httpx.HeaderRequestID), inCtx
}

func TestRequestID_KeepsValidIncoming(t *testing.T) {
	header, inCtx := serveRequestID(t, "abc-123")
	if header != "abc-123" {
		t.Errorf("response header = %q, want %q", header, "abc-123")
	}
	if inCtx != "abc-123" {
		t.Errorf("ctx request id = %q, want %q", inCtx, "abc-123")
	}
}

func TestRequestID_ReplacesJunk(t *testing.T) {
	for name, junk := range map[string]string{
		"too long":       strings.Repeat("a", 65),
		"bad characters": "bad id\n",
	} {
		t.Run(name, func(t *testing.T) {
			header, inCtx := serveRequestID(t, junk)
			if header == junk {
				t.Fatalf("junk request id was echoed back")
			}
			if !uuidRe.MatchString(header) {
				t.Errorf("response header = %q, want a UUIDv7", header)
			}
			if inCtx != header {
				t.Errorf("ctx request id = %q, want it to equal the header %q", inCtx, header)
			}
		})
	}
}

func TestRequestID_GeneratesWhenMissing(t *testing.T) {
	header, inCtx := serveRequestID(t, "")
	if !uuidRe.MatchString(header) {
		t.Errorf("response header = %q, want a UUIDv7", header)
	}
	if inCtx != header {
		t.Errorf("ctx request id = %q, want it to equal the header %q", inCtx, header)
	}
}

func TestRecover_PanicBecomes500Problem(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := httpx.Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var body struct {
		Code   string `json:"code"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body.Code != "internal" || body.Status != 500 {
		t.Errorf("body = %+v, want code=internal status=500", body)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("panic value leaked to the client: %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "boom") || !strings.Contains(logs.String(), "stack") {
		t.Errorf("panic not logged with its stack: %q", logs.String())
	}
}

func TestDeadline_SetsContextDeadline(t *testing.T) {
	var deadline time.Time
	var ok bool
	h := httpx.Deadline(5 * time.Second)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, ok = r.Context().Deadline()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !ok {
		t.Fatal("request context has no deadline")
	}
	if left := time.Until(deadline); left <= 0 || left > 5*time.Second {
		t.Errorf("deadline is %v away, want within (0, 5s]", left)
	}
}

func TestAccessLog_LogsOneLinePerRequest(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := httpx.AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/things", nil))

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %q", len(lines), logs.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if rec["method"] != "POST" || rec["path"] != "/v1/things" || rec["status"] != float64(http.StatusTeapot) {
		t.Errorf("log line = %v, want method=POST path=/v1/things status=418", rec)
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Errorf("log line has no duration_ms: %v", rec)
	}
}

func TestAccessLog_DefaultStatusIs200(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := httpx.AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hi") // no explicit WriteHeader
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !strings.Contains(logs.String(), `"status":200`) {
		t.Errorf("log = %q, want status 200", logs.String())
	}
}

func TestChain_FirstMiddlewareIsOutermost(t *testing.T) {
	var order []string
	mw := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := httpx.Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mw("a"), mw("b"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := strings.Join(order, ","); got != "a,b,handler" {
		t.Errorf("order = %s, want a,b,handler", got)
	}
}

func TestNewServer_SetsTimeouts(t *testing.T) {
	srv := httpx.NewServer(":1234", http.NotFoundHandler())
	if srv.Addr != ":1234" {
		t.Errorf("Addr = %q", srv.Addr)
	}
	if srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %v, want 60s", srv.IdleTimeout)
	}
}
