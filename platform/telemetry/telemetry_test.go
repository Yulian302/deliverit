package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/Yulian302/deliverit/platform/telemetry"
)

// Setup changes process-wide globals, so every test restores them when it
// ends. That keeps the tests independent of order (-shuffle=on). None of
// them may call t.Parallel().
func keepGlobals(t *testing.T) {
	t.Helper()
	tp, mp, prop := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		// Only restore what changed: setting a global to its current value
		// makes otel log a warning.
		if otel.GetTracerProvider() != tp {
			otel.SetTracerProvider(tp)
		}
		if otel.GetMeterProvider() != mp {
			otel.SetMeterProvider(mp)
		}
		otel.SetTextMapPropagator(prop)
	})
}

// keptExporter is an InMemoryExporter whose spans survive Shutdown.
// The stock one calls Reset() in Shutdown, and the provider's Shutdown (which
// we need, to flush the batch) shuts the exporter down too.
type keptExporter struct{ *tracetest.InMemoryExporter }

func (keptExporter) Shutdown(context.Context) error { return nil }

// setupInMemory runs Setup with an in-memory exporter and returns it.
func setupInMemory(t *testing.T) (*tracetest.InMemoryExporter, telemetry.Shutdown) {
	t.Helper()
	keepGlobals(t)
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Options{
		ServiceName:  "svc-test",
		SpanExporter: keptExporter{exp},
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return exp, shutdown
}

func TestSetup_ExportsSpansWithServiceName(t *testing.T) {
	exp, shutdown := setupInMemory(t)

	_, span := otel.Tracer("t").Start(context.Background(), "work")
	span.End()
	// Spans are batched in memory; Shutdown must flush them.
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	var name string
	for _, kv := range spans[0].Resource.Attributes() {
		if kv.Key == "service.name" {
			name = kv.Value.AsString()
		}
	}
	if name != "svc-test" {
		t.Errorf("resource service.name = %q, want %q", name, "svc-test")
	}
}

func TestSetup_PropagatorIsTraceContextAndBaggage(t *testing.T) {
	_, _ = setupInMemory(t)

	fields := otel.GetTextMapPropagator().Fields()
	for _, want := range []string{"traceparent", "baggage"} {
		if !slices.Contains(fields, want) {
			t.Errorf("propagator fields = %v, missing %q", fields, want)
		}
	}
}

func TestSetup_NoEndpointNoError(t *testing.T) {
	keepGlobals(t)
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Options{ServiceName: "svc-test"})
	if err != nil {
		t.Fatalf("Setup without endpoint: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown without endpoint: %v", err)
	}
}

// Without an exporter spans go nowhere, but they must still get real IDs:
// the logger writes trace_id from them, which is useful even locally.
func TestSetup_NoEndpointStillCreatesTraceIDs(t *testing.T) {
	keepGlobals(t)
	shutdown, err := telemetry.Setup(context.Background(), telemetry.Options{ServiceName: "svc-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	_, span := otel.Tracer("t").Start(context.Background(), "work")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Error("span has no trace ID; the global provider is still a no-op")
	}
}

func TestSetup_RequiresServiceName(t *testing.T) {
	keepGlobals(t)
	if _, err := telemetry.Setup(context.Background(), telemetry.Options{}); err == nil {
		t.Error("Setup with an empty ServiceName returned nil error")
	}
}

// The property the whole "one trace across services" goal depends on: an
// incoming traceparent is continued, not replaced by a new trace.
func TestHTTPHandler_ContinuesIncomingTrace(t *testing.T) {
	exp, shutdown := setupInMemory(t)

	const (
		traceID  = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentID = "00f067aa0ba902b7"
	)
	var inHandler trace.SpanContext
	h := telemetry.HTTPHandler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		inHandler = trace.SpanContextFromContext(r.Context())
	}), "test-op")

	req := httptest.NewRequest(http.MethodGet, "/v1/restaurants", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-"+parentID+"-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := inHandler.TraceID().String(); got != traceID {
		t.Errorf("handler ctx trace ID = %s, want the incoming %s", got, traceID)
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.SpanKind != trace.SpanKindServer {
		t.Errorf("span kind = %v, want server", s.SpanKind)
	}
	if got := s.Parent.SpanID().String(); got != parentID {
		t.Errorf("span parent = %s, want the caller's span %s", got, parentID)
	}
	if !s.Parent.IsRemote() {
		t.Error("span parent is not marked remote")
	}
}

// At the public edge a traceparent comes from the internet: anyone could
// attach their requests to arbitrary trace IDs or choose our sampling. So the
// public handler starts a NEW trace and only links to the caller's span.
func TestPublicHTTPHandler_StartsNewTraceLinkedToCaller(t *testing.T) {
	exp, shutdown := setupInMemory(t)

	const (
		clientTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		clientSpanID  = "00f067aa0ba902b7"
	)
	var inHandler trace.SpanContext
	h := telemetry.PublicHTTPHandler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		inHandler = trace.SpanContextFromContext(r.Context())
	}), "test-op")

	req := httptest.NewRequest(http.MethodGet, "/v1/restaurants", nil)
	req.Header.Set("traceparent", "00-"+clientTraceID+"-"+clientSpanID+"-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !inHandler.IsValid() {
		t.Fatal("handler has no span: the request isn't traced at all")
	}
	if inHandler.TraceID().String() == clientTraceID {
		t.Error("public handler continued the client's trace ID; it must start its own")
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Parent.IsValid() {
		t.Errorf("span has parent %s, want a new root", s.Parent.SpanID())
	}
	if len(s.Links) != 1 || s.Links[0].SpanContext.SpanID().String() != clientSpanID {
		t.Errorf("links = %v, want one link to the client's span %s", s.Links, clientSpanID)
	}
}
