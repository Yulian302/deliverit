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
	"net/http"
	"os"
	"time"

	"github.com/Yulian302/deliverit/platform/config"
	"github.com/Yulian302/deliverit/platform/health"
	"github.com/Yulian302/deliverit/platform/httpx"
	"github.com/Yulian302/deliverit/platform/logging"
	"github.com/Yulian302/deliverit/platform/run"
	"github.com/Yulian302/deliverit/platform/telemetry"
)

// settings is the Gateway's configuration. ORDER_URL joins it in Task 9,
// together with the Order client that uses it.
type settings struct {
	config.Common
}

const (
	// requestDeadline is the spec's public-edge limit: every request either
	// answers within 5s or gets a 504, never hangs.
	requestDeadline = 5 * time.Second
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

	// Application routes. GET /v1/restaurants is mounted here in Task 9.
	app := http.NewServeMux()

	// Order matters (first = outermost):
	//   telemetry  starts the span and puts trace IDs in ctx, so everything
	//              inside logs trace_id;
	//   RequestID  next, so the access log line carries request_id;
	//   AccessLog  outside Recover, so a panic is still logged (as a 500);
	//   Recover    turns panics into 500 problem+json;
	//   Deadline   innermost, closest to the handler that uses the ctx.
	handler := telemetry.HTTPHandler(httpx.Chain(app,
		httpx.RequestID,
		httpx.AccessLog(log),
		httpx.Recover(log),
		httpx.Deadline(requestDeadline),
	), cfg.ServiceName)

	srv := httpx.NewServer(cfg.HTTPAddr, withHealth(hc, handler))
	return run.ServeHTTP(ctx, log, srv, hc, cfg.ShutdownGrace)
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
