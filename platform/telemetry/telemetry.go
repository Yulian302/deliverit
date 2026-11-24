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
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
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
}

// Shutdown flushes buffered telemetry and stops the exporters.
type Shutdown func(context.Context) error

// Setup registers the global TracerProvider, MeterProvider and propagator.
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
func HTTPHandler(h http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(h, operation)
}
