// Package telemetry switches OpenTelemetry on for a service.
//
// Libraries (otelhttp, otelconnect, pgx tracing, the logger in
// platform/logging) only use the OpenTelemetry API, which does nothing until
// an SDK is registered globally. Setup is the one place that registers it:
//
//   - a TracerProvider: creates spans with real IDs, batches finished spans
//     in memory and exports them over OTLP gRPC;
//   - a MeterProvider: exports metrics over OTLP gRPC every 15s;
//   - the propagator (W3C TraceContext + Baggage): reads and writes the
//     traceparent/baggage headers so a trace continues across services.
//
// Call Setup first thing in main and defer the returned Shutdown so the last
// batch of spans is flushed after the server has drained.
//
// HTTPHandler must be the OUTERMOST middleware: it starts the span and puts
// it in the request context, and only code inside it (request ID, access
// log, handlers) sees that context and logs trace_id.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// metricInterval is how often metrics are pushed to the collector.
const metricInterval = 15 * time.Second

// Options configures Setup.
type Options struct {
	// ServiceName labels every span and metric (resource "service.name").
	// Required: without it Grafana shows everything as unknown_service.
	ServiceName string
	// Endpoint is the OTLP gRPC address, e.g. "otel-lgtm:4317". Empty means
	// nothing is exported (spans still get IDs for log correlation).
	Endpoint string
	// SpanExporter replaces the OTLP span exporter. Used by tests.
	SpanExporter sdktrace.SpanExporter
	// LogExporter replaces the OTLP log exporter. Used by tests.
	LogExporter sdklog.Exporter
}

// Shutdown flushes buffered telemetry and stops the exporters.
type Shutdown func(context.Context) error

// Setup registers the global TracerProvider, MeterProvider, LoggerProvider
// and propagator.
//
// It does not contact the collector: gRPC connects lazily, so a service
// starts fine while the collector is still coming up, and spans are retried
// or dropped instead of blocking requests.
func Setup(ctx context.Context, o Options) (Shutdown, error) {
	if o.ServiceName == "" {
		return nil, errors.New("telemetry: ServiceName is required")
	}

	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(), // sdk name/language/version
		resource.WithHost(),         // host.name: tells container replicas apart
		resource.WithAttributes(semconv.ServiceName(o.ServiceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	// Traces. Even with no exporter we install a real provider, so spans get
	// trace and span IDs and log lines can carry them.
	exp := o.SpanExporter
	if exp == nil && o.Endpoint != "" {
		exp, err = otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(o.Endpoint),
			otlptracegrpc.WithInsecure(), // plain gRPC inside the local Docker network
		)
		if err != nil {
			return nil, fmt.Errorf("telemetry: trace exporter: %w", err)
		}
	}
	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if exp != nil {
		// Batch: request goroutines only enqueue finished spans; a background
		// goroutine exports them, so the network never slows a request down.
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(tpOpts...)
	shutdowns := []func(context.Context) error{tp.Shutdown}

	// Metrics: only when there is somewhere to send them.
	if o.Endpoint != "" {
		mexp, err := otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithEndpoint(o.Endpoint),
			otlpmetricgrpc.WithInsecure(),
		)
		if err != nil {
			_ = tp.Shutdown(ctx) // don't leak what we already started
			return nil, fmt.Errorf("telemetry: metric exporter: %w", err)
		}
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp, sdkmetric.WithInterval(metricInterval))),
		)
		otel.SetMeterProvider(mp)
		shutdowns = append(shutdowns, mp.Shutdown)
	}

	// Logs: the global LoggerProvider receives every slog record through the
	// otelslog bridge (see platform/logging) and ships it over OTLP to Loki,
	// with the trace context of the span it was written in.
	lexp := o.LogExporter
	if lexp == nil && o.Endpoint != "" {
		lexp, err = otlploggrpc.New(ctx,
			otlploggrpc.WithEndpoint(o.Endpoint),
			otlploggrpc.WithInsecure(),
		)
		if err != nil {
			for _, s := range shutdowns {
				_ = s(ctx)
			}
			return nil, fmt.Errorf("telemetry: log exporter: %w", err)
		}
	}
	if lexp != nil {
		lp := sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			// Batch, like spans: a log call never waits on the network.
			sdklog.WithProcessor(sdklog.NewBatchProcessor(lexp)),
		)
		otel.SetLoggerProvider(lp)
		// Flushed BEFORE the tracer provider (shutdowns run in order), so
		// spans written while flushing logs still go out.
		shutdowns = append([]func(context.Context) error{lp.Shutdown}, shutdowns...)
	}

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, // traceparent / tracestate
		propagation.Baggage{},      // baggage
	))

	return func(ctx context.Context) error {
		// Shut down every provider even if one fails, and report all errors.
		var errs []error
		for _, s := range shutdowns {
			errs = append(errs, s(ctx))
		}
		return errors.Join(errs...)
	}, nil
}

// HTTPHandler wraps h so every incoming request gets a SERVER span that
// continues the caller's trace (from traceparent) and records HTTP request
// metrics. operation names the span when no route pattern is known.
//
// Use it for internal endpoints, called only by our own services. For
// endpoints reachable from the internet, use PublicHTTPHandler.
func HTTPHandler(h http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(h, operation)
}

// PublicHTTPHandler is HTTPHandler for internet-facing endpoints: it never
// continues a caller's trace. Each request starts a NEW trace whose root span
// links to the caller's span (if any), so the connection stays visible.
//
// Why: a traceparent from the internet is untrusted input. Continuing it
// would let anyone attach their requests to arbitrary trace IDs, or choose
// our sampling decision (its "sampled" flag). Trust inside, distrust at the
// edge.
//
// It also returns the trace ID in the X-Trace-Id response header (see
// HeaderTraceID).
func PublicHTTPHandler(h http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(exposeTraceID(h), operation,
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }),
	)
}

// HeaderTraceID carries the request's trace ID back to the client. Anyone
// reporting a problem (or the smoke test) can quote it, and Tempo opens
// exactly that trace. Safe to expose: it's a random ID, not data.
const HeaderTraceID = "X-Trace-Id"

// exposeTraceID sets HeaderTraceID before the handler writes its response
// (headers can't change after the first write).
func exposeTraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
			w.Header().Set(HeaderTraceID, sc.TraceID().String())
		}
		next.ServeHTTP(w, r)
	})
}

// Route wraps a handler registered on a ServeMux so its request's span is
// named after the matched route ("GET /v1/restaurants") with the standard
// http.route attribute.
//
// Why it's needed: the span starts before routing (tracing must be the
// outermost middleware), and the middlewares in between copy the request
// (r.WithContext), so the route the mux finds never reaches the request the
// tracing handler holds. Without this, spans are named just "GET".
//
// Anything in front of the tracing handler must not be a ServeMux (see
// health.WithProbes): otelhttp renames the span from the pattern on its own
// request after the handler returns, which would overwrite this name.
//
//	app.Handle("GET /v1/restaurants", telemetry.Route(handler))
func Route(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Pattern != "" {
			// r.Pattern is "GET /v1/restaurants"; http.route is the path part.
			route := r.Pattern
			if _, path, ok := strings.Cut(route, " "); ok {
				route = path
			}
			span := trace.SpanFromContext(r.Context())
			span.SetName(r.Method + " " + route)
			span.SetAttributes(semconv.HTTPRoute(route))
			// Same for the request metrics (duration histogram per route).
			if l, ok := otelhttp.LabelerFromContext(r.Context()); ok {
				l.Add(semconv.HTTPRoute(route))
			}
		}
		h.ServeHTTP(w, r)
	})
}
