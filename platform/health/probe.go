package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// probeTimeout bounds one health probe. Docker's own healthcheck timeout
// (compose) is longer, so the probe reports a hung service as unhealthy
// instead of being killed mid-call.
const probeTimeout = 3 * time.Second

// ProbeLocal calls this process's own /readyz over loopback and returns nil
// only if it answers 200. It backs the `healthcheck` subcommand: the runtime
// image is distroless (no shell, no curl), so Docker's HEALTHCHECK runs the
// service binary itself:
//
//	order healthcheck   → exit 0 when ready, 1 otherwise
//
// httpAddr is the service's listen address (HTTP_ADDR), e.g. ":8081".
func ProbeLocal(ctx context.Context, httpAddr string) error {
	url, err := LocalReadyURL(httpAddr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("health probe: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health probe %s: %w", url, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health probe %s: status %d", url, resp.StatusCode)
	}
	return nil
}

// LocalReadyURL turns a listen address into the loopback /readyz URL:
// ":8081", "0.0.0.0:8081" and "[::]:8081" all mean "every interface", and
// inside the container the probe reaches them on 127.0.0.1.
func LocalReadyURL(httpAddr string) (string, error) {
	host, port, err := net.SplitHostPort(httpAddr)
	if err != nil {
		return "", fmt.Errorf("health probe: bad listen address %q: %w", httpAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/readyz", nil
}

// WithProbes serves /livez and /readyz directly and passes every other
// request to app. Put it OUTSIDE the middleware chain: an orchestrator probes
// every few seconds, and tracing or access-logging each probe would bury
// real traffic.
//
// A plain switch, not a ServeMux, on purpose: a mux would stamp its pattern
// ("/") on the request, and the tracing middleware inside app would then
// rename every request's span to "GET /" (see telemetry.Route).
func WithProbes(hc *Checker, app http.Handler) http.Handler {
	live, ready := hc.LiveHandler(), hc.ReadyHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			switch r.URL.Path {
			case "/livez":
				live.ServeHTTP(w, r)
				return
			case "/readyz":
				ready.ServeHTTP(w, r)
				return
			}
		}
		app.ServeHTTP(w, r)
	})
}
