// Command gateway runs the public HTTP API: the only service browsers and
// apps talk to. It validates requests, calls the internal services over
// Connect-RPC and turns their errors into problem+json responses.
//
//	gateway              serve the public API, /livez, /readyz
//	gateway healthcheck  exit 0 if this container's Gateway is ready (Docker HEALTHCHECK)
//	gateway keygen       print a new random JWT_SIGNING_KEY and exit (needs no config)
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
	"github.com/Yulian302/deliverit/platform/identity"
	"github.com/Yulian302/deliverit/platform/logging"
	"github.com/Yulian302/deliverit/platform/run"
	"github.com/Yulian302/deliverit/platform/telemetry"
	"github.com/Yulian302/deliverit/services/gateway/internal/api"
	"github.com/Yulian302/deliverit/services/gateway/internal/auth"
)

// settings is the Gateway's configuration.
type settings struct {
	config.Common
	// OrderURL is the Order service's base URL, e.g. http://order:8081.
	OrderURL string `env:"ORDER_URL,required,notEmpty"`
	// JWTSigningKey signs and verifies login tokens: standard base64 of a
	// 32-byte Ed25519 seed (`gateway keygen` makes one). Required, so a
	// Gateway can never start unable to check tokens.
	JWTSigningKey string `env:"JWT_SIGNING_KEY,required,notEmpty"`
	// JWTTTL is how long a login token stays valid.
	JWTTTL time.Duration `env:"JWT_TTL" envDefault:"12h"`
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
	mode := "serve"
	switch {
	case len(args) == 1 && args[0] == "healthcheck":
		mode = "healthcheck"
	case len(args) == 1 && args[0] == "keygen":
		// Before config: generating a key must not require having one.
		key, err := auth.GenerateSigningKey()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, key)
		return err
	case len(args) > 0:
		return fmt.Errorf("unexpected arguments %q (want none, or: healthcheck, keygen)", args)
	}
	var cfg settings
	if err := config.Load(&cfg); err != nil {
		return err
	}
	if mode == "healthcheck" {
		// What Docker's HEALTHCHECK runs (distroless has no curl).
		return health.ProbeLocal(ctx, cfg.HTTPAddr)
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

	key, err := auth.ParseSigningKey(cfg.JWTSigningKey)
	if err != nil {
		return err
	}
	issuer := auth.NewIssuer(key, cfg.JWTTTL, time.Now)

	order, err := newOrderClient(cfg.OrderURL)
	if err != nil {
		return err
	}

	// Application routes (the public contract is api/openapi.yaml).
	// Method-specific patterns: other methods get 405 from the mux.
	app := http.NewServeMux()
	app.Handle("GET /v1/restaurants", telemetry.Route(api.NewRestaurantsHandler(order, log)))
	app.Handle("POST /v1/auth/demo-login", telemetry.Route(api.NewDemoLoginHandler(issuer)))
	app.Handle("GET /v1/auth/me", telemetry.Route(api.RequireRole()(api.NewMeHandler())))
	app.Handle("GET /v1/restaurants/{id}/menu", telemetry.Route(api.NewMenuHandler(order, log)))
	app.Handle("POST /v1/orders", telemetry.Route(api.RequireRole(identity.RoleCustomer)(api.NewPlaceOrderHandler(order, log))))
	app.Handle("GET /v1/orders/{id}", telemetry.Route(api.RequireRole()(api.NewGetOrderHandler(order, log))))

	// Order matters (first = outermost):
	//   telemetry  starts the span and puts trace IDs in ctx, so everything
	//              inside logs trace_id. PUBLIC: an incoming traceparent from
	//              the internet starts a new, linked trace, never continued;
	//   RequestID  next, so the access log line carries request_id;
	//   AccessLog  outside Recover, so a panic is still logged (as a 500);
	//   Recover    turns panics into 500 problem+json;
	//   Authenticate verifies a bearer token (if any) and puts the caller's
	//              identity in ctx; routes that need one add RequireRole;
	//   Deadline   innermost, closest to the handler that uses the ctx.
	handler := telemetry.PublicHTTPHandler(httpx.Chain(app,
		httpx.RequestID,
		httpx.AccessLog(log),
		httpx.Recover(log),
		api.Authenticate(issuer),
		httpx.Deadline(requestDeadline),
	), cfg.ServiceName)

	srv := httpx.NewServer(cfg.HTTPAddr, health.WithProbes(hc, handler))
	return run.ServeHTTP(ctx, log, srv, hc, cfg.ShutdownGrace)
}

// newOrderClient builds the Connect client for the Order service.
//
//   - HTTP/2 without TLS (h2c) inside the private network, as Order serves.
//   - A short dial timeout (see upstreamDialTimeout): unreachable → fast 503.
//   - No overall client timeout: each call is bounded by the request's
//     context (5s Deadline middleware), and Connect sends the remaining time
//     to Order in a header, so Order stops when the Gateway gives up.
//   - identity.Interceptor sends the verified caller (from ctx) to Order in
//     Deliverit-* headers. It builds them from ctx only: headers from the
//     public request are never copied onto the RPC.
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
	return orderv1connect.NewOrderServiceClient(httpClient, baseURL, connect.WithInterceptors(tracing, identity.Interceptor())), nil
}
