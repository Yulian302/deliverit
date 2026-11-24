package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
	"github.com/Yulian302/deliverit/platform/logging"
	"github.com/Yulian302/deliverit/platform/pg"
)

// h2cClient speaks HTTP/2 without TLS, like the Gateway's client will.
func h2cClient() *http.Client {
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: protos}}
}

// The real binary serves OrderService. lat=100 is rejected by validation
// before any DB access, so this needs no database: a 404 or Unimplemented
// here would mean the handler isn't mounted.
func TestOrder_ServesListRestaurants(t *testing.T) {
	url, _, _ := startOrder(t, "postgres://u:p@127.0.0.1:1/db")
	client := orderv1connect.NewOrderServiceClient(h2cClient(), url)

	_, err := client.ListRestaurants(context.Background(), &orderv1.ListRestaurantsRequest{Lat: 100})

	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("code = %v (err %v), want invalid_argument from the real handler", got, err)
	}
}

// tracedCall serves catalogRoute with an in-memory tracer and our real
// logger, makes one call from inside a caller span (propagated in the
// traceparent header, as the Gateway will), and returns the caller span, the
// recorded spans and the service's log output.
func tracedCall(t *testing.T) (caller trace.Span, spans tracetest.SpanStubs, logs *bytes.Buffer) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	logs = new(bytes.Buffer)
	log, err := logging.New(logs, "order", "info")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pg.Connect(context.Background(), "postgres://u:p@127.0.0.1:1/db") // never dialled here
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	path, handler, err := catalogRoute(pool, log)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, caller := tp.Tracer("gateway").Start(context.Background(), "caller")
	client := orderv1connect.NewOrderServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(propagate{}))
	_, _ = client.ListRestaurants(ctx, &orderv1.ListRestaurantsRequest{Lat: 100}) // rejected before any DB access
	caller.End()
	return caller, exp.GetSpans(), logs
}

// Slice 1's goal is ONE trace across Gateway → Order. otelconnect's server
// default is to distrust the caller's trace and start a new one (linked, not
// nested). Order's only callers are our own services, so it must continue
// the caller's trace: its server span is a child of the caller's span.
func TestCatalogRoute_ContinuesCallerTrace(t *testing.T) {
	caller, spans, _ := tracedCall(t)

	var server *tracetest.SpanStub
	for _, s := range spans {
		if s.SpanKind == trace.SpanKindServer {
			server = &s
		}
	}
	if server == nil {
		t.Fatal("no server span recorded: the otelconnect interceptor is not installed")
	}
	if server.SpanContext.TraceID() != caller.SpanContext().TraceID() {
		t.Errorf("server span is in trace %s, want the caller's trace %s (a new trace was started)",
			server.SpanContext.TraceID(), caller.SpanContext().TraceID())
	}
	if server.Parent.SpanID() != caller.SpanContext().SpanID() {
		t.Errorf("server span parent = %s, want the caller's span %s", server.Parent.SpanID(), caller.SpanContext().SpanID())
	}
}

// Each RPC gets one access-log line with its RPC code, written inside the
// server span so it carries the caller's trace_id: from a log line in Loki
// you can jump to the trace in Tempo. This fails if the log interceptor runs
// before (outside) the tracing one.
func TestCatalogRoute_RPCLogCarriesTraceID(t *testing.T) {
	caller, _, logs := tracedCall(t)

	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(l), &rec) == nil && rec["msg"] == "rpc" {
			line = rec
		}
	}
	if line == nil {
		t.Fatalf("no rpc access-log line in %q", logs.String())
	}
	if line["code"] != "invalid_argument" || line["procedure"] != orderv1connect.OrderServiceListRestaurantsProcedure {
		t.Errorf("rpc log = %v, want code=invalid_argument procedure=%s", line, orderv1connect.OrderServiceListRestaurantsProcedure)
	}
	if want := caller.SpanContext().TraceID().String(); line["trace_id"] != want {
		t.Errorf("rpc log trace_id = %v, want the caller's %s", line["trace_id"], want)
	}
}

// propagate injects the current trace into outgoing request headers, the
// job the Gateway's otelconnect client interceptor will do in Task 9.
type propagate struct{}

func (propagate) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header()))
		return next(ctx, req)
	}
}

func (propagate) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (propagate) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
