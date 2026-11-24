// Command gateway runs the public HTTP API: the only service browsers and
// apps talk to. It validates requests, calls the internal services over
// Connect-RPC and turns their errors into problem+json responses.
//
// Configuration comes from environment variables only (see settings).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
	"github.com/Yulian302/deliverit/platform/config"
	"github.com/Yulian302/deliverit/platform/health"
	"github.com/Yulian302/deliverit/platform/httpx"
	"github.com/Yulian302/deliverit/platform/logging"
	"github.com/Yulian302/deliverit/platform/run"
	"github.com/Yulian302/deliverit/platform/telemetry"
	"github.com/Yulian302/deliverit/services/gateway/internal/api"
)

// settings is the Gateway's configuration.
type settings struct {
	config.Common
	// OrderURL is the Order service's base URL, e.g. http://order:8081.
	OrderURL string `env:"ORDER_URL,required,notEmpty"`
}

const (
	// requestDeadline is the spec's public-edge limit: every request either
	// answers within 5s or gets a 504, never hangs.
	requestDeadline = 5 * time.Second
	// upstreamDialTimeout bounds opening a connection to an internal service.
	// If a host is down in a way that drops packets (crashed VM, firewall),
	// a connect attempt hangs instead of being refused; without this limit
	// it would eat the whole 5s request budget and end as a 504. With it, an
	// unreachable Order is a fast 503 upstream_unavailable.
	upstreamDialTimeout = 1 * time.Second
	// telemetryFlushTimeout bounds how long exit waits for the last spans.
	telemetryFlushTimeout = 5 * time.Second
)

func main() {
	ctx, stop := run.SignalContext()
	err := start(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

// start is main without the process-level parts (signals, exit codes), so
// tests can run the whole service in-process and stop it by cancelling ctx.
func start(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected arguments %q (gateway takes none)", args)
	}
	var cfg settings
	if err := config.Load(&cfg); err != nil {
		return err
	}
	log, err := logging.New(stdout, cfg.ServiceName, cfg.LogLevel)
	if err != nil {
		return err
	}
	return serve(ctx, cfg, log)
}

// serve wires the service and blocks until ctx is cancelled and the server
// has drained, then flushes telemetry.
func serve(ctx context.Context, cfg settings, log *slog.Logger) (err error) {
	shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.Options{
		ServiceName: cfg.ServiceName,
		Endpoint:    cfg.OTLPEndpoint,
	})
	if err != nil {
		return err
	}
	defer func() {
		// ctx is already cancelled here; flush on a fresh deadline.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryFlushTimeout)
		defer cancel()
		err = errors.Join(err, shutdownTelemetry(fctx))
	}()

	// No readiness checks yet. The Gateway deliberately won't check Order
	// either (Task 9): if Order is down, the Gateway still answers with 503
	// problem+json, which is better than being taken out of rotation.
	hc := health.New()

	order, err := newOrderClient(cfg.OrderURL)
	if err != nil {
		return err
	}

	// Application routes (the public contract is api/openapi.yaml).
	// Method-specific patterns: other methods get 405 from the mux.
	app := http.NewServeMux()
	app.Handle("GET /v1/restaurants", api.NewRestaurantsHandler(order, log))

	// Order matters (first = outermost):
	//   telemetry  starts the span and puts trace IDs in ctx, so everything
	//              inside logs trace_id. PUBLIC: an incoming traceparent from
	//              the internet starts a new, linked trace, never continued;
	//   RequestID  next, so the access log line carries request_id;
	//   AccessLog  outside Recover, so a panic is still logged (as a 500);
	//   Recover    turns panics into 500 problem+json;
	//   Deadline   innermost, closest to the handler that uses the ctx.
	handler := telemetry.PublicHTTPHandler(httpx.Chain(app,
		httpx.RequestID,
		httpx.AccessLog(log),
		httpx.Recover(log),
		httpx.Deadline(requestDeadline),
	), cfg.ServiceName)

	srv := httpx.NewServer(cfg.HTTPAddr, withHealth(hc, handler))
	return run.ServeHTTP(ctx, log, srv, hc, cfg.ShutdownGrace)
}

// newOrderClient builds the Connect client for the Order service.
//
//   - HTTP/2 without TLS (h2c) inside the private network, as Order serves.
//   - A short dial timeout (see upstreamDialTimeout): unreachable → fast 503.
//   - No overall client timeout: each call is bounded by the request's
//     context (5s Deadline middleware), and Connect sends the remaining time
//     to Order in a header, so Order stops when the Gateway gives up.
//   - otelconnect creates the CLIENT span and injects traceparent, so Order's
//     server span joins this trace. (No otelhttp transport as well: that
//     would add a second, redundant span per call.)
func newOrderClient(baseURL string) (orderv1connect.OrderServiceClient, error) {
	tracing, err := otelconnect.NewInterceptor()
	if err != nil {
		return nil, fmt.Errorf("otelconnect: %w", err)
	}
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	httpClient := &http.Client{Transport: &http.Transport{
		Protocols:   protocols,
		DialContext: (&net.Dialer{Timeout: upstreamDialTimeout}).DialContext,
	}}
	return orderv1connect.NewOrderServiceClient(httpClient, baseURL, connect.WithInterceptors(tracing)), nil
}

// withHealth serves the probes directly and everything else through app.
// Probes skip the middleware on purpose: an orchestrator hits them every few
// seconds, and access-logging or tracing each hit would bury real traffic.
func withHealth(hc *health.Checker, app http.Handler) http.Handler {
	root := http.NewServeMux()
	root.Handle("GET /livez", hc.LiveHandler())
	root.Handle("GET /readyz", hc.ReadyHandler())
	root.Handle("/", app)
	return root
}
