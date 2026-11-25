// Command order runs the Order service.
//
//	order            serve: OrderService RPC API, /livez, /readyz
//	order migrate    wait for Postgres, apply schema migrations, exit
//	order healthcheck   exit 0 if this container's Order is ready (Docker HEALTHCHECK)
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

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
	"github.com/Yulian302/deliverit/platform/config"
	"github.com/Yulian302/deliverit/platform/health"
	"github.com/Yulian302/deliverit/platform/httpx"
	"github.com/Yulian302/deliverit/platform/logging"
	"github.com/Yulian302/deliverit/platform/pg"
	"github.com/Yulian302/deliverit/platform/rpcx"
	"github.com/Yulian302/deliverit/platform/run"
	"github.com/Yulian302/deliverit/platform/telemetry"
	"github.com/Yulian302/deliverit/services/order/db/migrations"
	"github.com/Yulian302/deliverit/services/order/internal/catalog"
)

// settings is Order's configuration: the shared fields plus its own.
type settings struct {
	config.Common
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
	// MigrateWait is how long `order migrate` waits for Postgres to accept
	// connections (in compose it often starts before Postgres is ready).
	MigrateWait time.Duration `env:"MIGRATE_WAIT" envDefault:"60s"`
}

// telemetryFlushTimeout bounds how long exit waits for the last spans.
const telemetryFlushTimeout = 5 * time.Second

func main() {
	ctx, stop := run.SignalContext()
	err := start(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "order:", err)
		os.Exit(1)
	}
}

// start is main without the process-level parts (signals, exit codes), so
// tests can run the whole service in-process and stop it by cancelling ctx.
func start(ctx context.Context, args []string, stdout io.Writer) error {
	var cfg settings
	if err := config.Load(&cfg); err != nil {
		return err
	}
	log, err := logging.New(stdout, cfg.ServiceName, cfg.LogLevel)
	if err != nil {
		return err
	}

	mode := "serve"
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case "serve":
		return serve(ctx, cfg, log)
	case "migrate":
		return migrate(ctx, cfg, log)
	case "healthcheck":
		return health.ProbeLocal(ctx, cfg.HTTPAddr)
	default:
		return fmt.Errorf("unknown mode %q (want: serve | migrate | healthcheck)", mode)
	}
}

// serve wires the service and blocks until ctx is cancelled and the server
// has drained. Teardown runs in reverse order of setup (defers): pool closed,
// then telemetry flushed, so spans from the last requests are not lost.
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

	// No ping: if Postgres is down we still start, report /readyz 503, and
	// become ready on our own once it is reachable (Review Focus 1).
	pool, err := pg.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	hc := health.New()
	hc.Add("postgres", pg.ReadyCheck(pool))

	// Application routes: the Connect-RPC API.
	app := http.NewServeMux()
	path, rpc, err := catalogRoute(pool, log)
	if err != nil {
		return err
	}
	app.Handle(path, rpc)

	// No httpx.AccessLog here: every app route is an RPC, and rpcx.AccessLog
	// (inside catalogRoute) logs each call with its RPC code and trace_id.
	// An HTTP-level line as well would double every call in the logs.
	//
	// No Deadline middleware either: callers set the deadline, and Connect
	// carries it in the request (Connect-Timeout-Ms / grpc-timeout header).
	handler := httpx.Chain(app,
		httpx.RequestID,
		httpx.Recover(log),
	)

	srv := httpx.NewServer(cfg.HTTPAddr, health.WithProbes(hc, handler))
	// Connect-RPC clients use HTTP/2; inside the private network it runs
	// without TLS ("h2c"). HTTP/1.1 stays on for curl and probes.
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)

	return run.ServeHTTP(ctx, log, srv, hc, cfg.ShutdownGrace)
}

// catalogRoute builds the OrderService RPC endpoint: handler → service →
// Postgres store, with OpenTelemetry on every call. It returns the URL path
// prefix to mount it on (/deliverit.order.v1.OrderService/).
func catalogRoute(pool *pgxpool.Pool, log *slog.Logger) (string, http.Handler, error) {
	// otelconnect creates a SERVER span per RPC plus RPC metrics.
	//
	// WithTrustRemote: by default otelconnect distrusts the caller's trace
	// and starts a new one (linked, not nested), which suits a public API
	// where anyone can send a traceparent header. Order is internal and only
	// called by our own services, so it continues their trace instead: that
	// is what makes Gateway → Order one trace in Grafana.
	tracing, err := otelconnect.NewInterceptor(otelconnect.WithTrustRemote())
	if err != nil {
		return "", nil, fmt.Errorf("otelconnect: %w", err)
	}

	svc := catalog.NewService(catalog.NewPostgresStore(pool))
	path, h := orderv1connect.NewOrderServiceHandler(
		catalog.NewHandler(svc, log),
		// First = outermost: tracing starts the span, then the access log
		// runs inside it, so its line carries trace_id.
		connect.WithInterceptors(tracing, rpcx.AccessLog(log)),
	)
	return path, h, nil
}

// migrate waits until Postgres accepts connections (up to cfg.MigrateWait),
// then applies all pending migrations.
func migrate(ctx context.Context, cfg settings, log *slog.Logger) error {
	wctx, cancel := context.WithTimeout(ctx, cfg.MigrateWait)
	defer cancel()
	if err := waitForDB(wctx, cfg.DatabaseURL, log); err != nil {
		return fmt.Errorf("migrate: postgres not reachable within %s: %w", cfg.MigrateWait, err)
	}

	if err := pg.Migrate(ctx, cfg.DatabaseURL, migrations.FS); err != nil {
		return err
	}
	log.InfoContext(ctx, "migrations applied")
	return nil
}

// waitForDB tries to connect once a second until it succeeds or ctx ends.
func waitForDB(ctx context.Context, dsn string, log *slog.Logger) error {
	for {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			return conn.Close(ctx)
		}
		// pgx connect errors name host, user and database, never the password.
		log.WarnContext(ctx, "postgres not reachable yet, retrying", slog.Any("error", err))

		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
}
