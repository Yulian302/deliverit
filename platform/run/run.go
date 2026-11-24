// Package run owns a service's lifecycle: serve until told to stop, then shut
// down gracefully so no in-flight request is dropped.
//
// Shutdown order matters:
//  1. readiness turns 503 (hc.SetDraining) so load balancers stop sending
//     new requests to this instance;
//  2. srv.Shutdown closes the listener (new connections are refused) and
//     waits for in-flight requests to finish;
//  3. if they don't finish within the grace period, the remaining
//     connections are closed forcibly and Serve reports the timeout.
package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Yulian302/deliverit/platform/health"
)

// SignalContext returns a context that is cancelled on SIGINT (Ctrl-C) or
// SIGTERM (what Docker and Kubernetes send to stop a container).
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ServeHTTP listens on srv.Addr and then behaves like Serve.
func ServeHTTP(ctx context.Context, log *slog.Logger, srv *http.Server, hc *health.Checker, grace time.Duration) error {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", srv.Addr, err)
	}
	return Serve(ctx, log, srv, ln, hc, grace)
}

// Serve serves on ln until ctx is cancelled, then shuts down gracefully
// (see the package comment). It returns nil after a clean shutdown, an
// error wrapping context.DeadlineExceeded if requests outlived the grace
// period, or the server's error if serving failed before ctx was cancelled.
func Serve(ctx context.Context, log *slog.Logger, srv *http.Server, ln net.Listener, hc *health.Checker, grace time.Duration) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.InfoContext(ctx, "listening", slog.String("addr", ln.Addr().String()))

	select {
	case err := <-serveErr:
		// Serve stopped by itself, before anyone asked it to.
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	log.InfoContext(ctx, "shutting down", slog.Duration("grace", grace))
	hc.SetDraining()

	// ctx is already cancelled, so the grace timer needs a fresh deadline.
	// WithoutCancel keeps ctx's values (e.g. trace info) for logging.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Grace period ran out: cut the remaining connections rather than
		// leave them hanging after the process is meant to be gone.
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown did not finish within %s: %w", grace, err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	log.InfoContext(ctx, "shutdown complete")
	return nil
}
