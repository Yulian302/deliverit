package telemetry_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/Yulian302/deliverit/platform/logging"
	"github.com/Yulian302/deliverit/platform/telemetry"
)

// memLogs is a log exporter that keeps every record in memory.
type memLogs struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (m *memLogs) Export(_ context.Context, rs []sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rs {
		m.recs = append(m.recs, r.Clone())
	}
	return nil
}
func (*memLogs) Shutdown(context.Context) error   { return nil }
func (*memLogs) ForceFlush(context.Context) error { return nil }

func (m *memLogs) bodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.recs))
	for _, r := range m.recs {
		out = append(out, r.Body().AsString())
	}
	return out
}

// keepLogGlobal restores the global LoggerProvider after the test.
func keepLogGlobal(t *testing.T) {
	t.Helper()
	prev := otel.GetLoggerProvider()
	t.Cleanup(func() { otel.SetLoggerProvider(prev) })
}

// Logs travel to Loki over OTLP, carrying the trace context of the span they
// were written in, so Grafana can jump between a log line and its trace.
func TestSetup_ExportsLogsWithTraceContext(t *testing.T) {
	keepGlobals(t)
	keepLogGlobal(t)
	logs := &memLogs{}
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Options{
		ServiceName: "svc-test",
		LogExporter: logs,
	})
	if err != nil {
		t.Fatal(err)
	}
	log, err := logging.New(io.Discard, "svc-test", "info")
	if err != nil {
		t.Fatal(err)
	}

	ctx, span := otel.Tracer("t").Start(context.Background(), "work")
	log.InfoContext(ctx, "hello", "k", "v")
	span.End()
	if err := shutdown(context.Background()); err != nil { // flushes the log batch
		t.Fatal(err)
	}

	if got := logs.bodies(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("exported log bodies = %q, want exactly [hello]", got)
	}
	r := logs.recs[0]
	if r.TraceID() != span.SpanContext().TraceID() {
		t.Errorf("log trace ID = %s, want the span's %s", r.TraceID(), span.SpanContext().TraceID())
	}
	var service string
	for _, kv := range r.Resource().Attributes() {
		if kv.Key == "service.name" {
			service = kv.Value.AsString()
		}
	}
	if service != "svc-test" {
		t.Errorf("log resource service.name = %q, want svc-test", service)
	}
}

// LOG_LEVEL applies to exported logs too: a debug line hidden on stdout must
// not be shipped (and paid for) in Loki either.
func TestSetup_ExportedLogsRespectLevel(t *testing.T) {
	keepGlobals(t)
	keepLogGlobal(t)
	logs := &memLogs{}
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Options{ServiceName: "svc-test", LogExporter: logs})
	if err != nil {
		t.Fatal(err)
	}
	log, err := logging.New(io.Discard, "svc-test", "warn")
	if err != nil {
		t.Fatal(err)
	}

	log.Debug("debug line")
	log.Info("info line")
	log.Warn("warn line")
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := logs.bodies(); len(got) != 1 || got[0] != "warn line" {
		t.Errorf("exported %q, want only [warn line] at level warn", got)
	}
}

// The services' mains create the logger BEFORE telemetry.Setup. That works
// because the default global LoggerProvider forwards to the first real one
// installed. That forwarding is set ONCE per process and can't be undone, so
// this runs in a fresh child process: in the shared test process an earlier
// test's (already shut down) provider would have taken that one slot.
func TestSetup_LoggerCreatedBeforeSetupStillExports(t *testing.T) {
	if os.Getenv("DELIVERIT_FRESH_PROCESS") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSetup_LoggerCreatedBeforeSetupStillExports$", "-test.count=1")
		cmd.Env = append(os.Environ(), "DELIVERIT_FRESH_PROCESS=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fresh-process run failed: %v\n%s", err, out)
		}
		return
	}

	log, err := logging.New(io.Discard, "svc-test", "info") // first, like main
	if err != nil {
		t.Fatal(err)
	}
	logs := &memLogs{}
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Options{ServiceName: "svc-test", LogExporter: logs})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("created before setup")
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := logs.bodies(); len(got) != 1 || got[0] != "created before setup" {
		t.Fatalf("exported %q, want [created before setup]", got)
	}
}
