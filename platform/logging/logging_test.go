package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/Yulian302/deliverit/platform/logging"
)

var (
	testTraceID = trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	testSpanID  = trace.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}
)

// ctxWithSpan returns a context carrying a valid (sampled) span context,
// the same thing OpenTelemetry puts there while a span is active.
func ctxWithSpan() context.Context {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    testTraceID,
		SpanID:     testSpanID,
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), sc)
}

// decode parses exactly one JSON log line.
func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1:\n%s", len(lines), buf)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decode %q: %v", lines[0], err)
	}
	return rec
}

func newLogger(t *testing.T, buf *bytes.Buffer, level string) *slog.Logger {
	t.Helper()
	log, err := logging.New(buf, "order", level)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return log
}

func TestLogger_AddsTraceAndRequestID(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(t, &buf, "info")

	ctx := logging.WithRequestID(ctxWithSpan(), "req-1")
	log.InfoContext(ctx, "hi", "order_id", "o-1")

	rec := decode(t, &buf)
	want := map[string]any{
		"msg":        "hi",
		"service":    "order",
		"order_id":   "o-1",
		"trace_id":   testTraceID.String(),
		"span_id":    testSpanID.String(),
		"request_id": "req-1",
	}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("%s = %v, want %v", k, rec[k], v)
		}
	}
}

func TestLogger_NoTraceFieldsWithoutSpan(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(t, &buf, "info")

	log.InfoContext(context.Background(), "hi")

	rec := decode(t, &buf)
	for _, k := range []string{"trace_id", "span_id", "request_id"} {
		if _, ok := rec[k]; ok {
			t.Errorf("unexpected %s = %v", k, rec[k])
		}
	}
	if rec["service"] != "order" {
		t.Errorf("service = %v, want order", rec["service"])
	}
}

func TestLogger_WithKeepsTraceFields(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(t, &buf, "info")

	// .With and .WithGroup create new handlers; the trace fields must survive.
	child := log.With("component", "catalog").WithGroup("req")
	child.InfoContext(ctxWithSpan(), "hi", "path", "/x")

	rec := decode(t, &buf)
	if rec["trace_id"] != testTraceID.String() {
		t.Errorf("trace_id = %v, want %v", rec["trace_id"], testTraceID.String())
	}
	if rec["component"] != "catalog" {
		t.Errorf("component = %v, want catalog", rec["component"])
	}
}

func TestLogger_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(t, &buf, "warn")

	log.InfoContext(context.Background(), "dropped")
	if buf.Len() != 0 {
		t.Fatalf("info record written at warn level: %s", buf.String())
	}

	log.WarnContext(context.Background(), "kept")
	if rec := decode(t, &buf); rec["msg"] != "kept" {
		t.Errorf("msg = %v, want kept", rec["msg"])
	}
}

func TestNew_RejectsUnknownLevel(t *testing.T) {
	_, err := logging.New(&bytes.Buffer{}, "order", "verbose")
	if err == nil || !strings.Contains(err.Error(), "verbose") {
		t.Fatalf("New error = %v, want one mentioning the bad level", err)
	}
}

func TestRequestIDFrom_EmptyWhenUnset(t *testing.T) {
	if id := logging.RequestIDFrom(context.Background()); id != "" {
		t.Fatalf("RequestIDFrom = %q, want empty", id)
	}
}
