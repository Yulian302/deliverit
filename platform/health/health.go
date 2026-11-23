// Package health serves the two probes every DeliverIt service exposes:
//
//   - /livez  — "is the process alive?" Always 200 while the process can
//     answer HTTP. An orchestrator restarts the process when this fails,
//     so it must NOT depend on databases or other services (a DB outage
//     would otherwise restart every healthy instance at once).
//   - /readyz — "should this instance get traffic right now?" 503 when a
//     dependency check fails or the instance is draining during shutdown,
//     so load balancers stop sending it new requests.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// checkTimeout bounds each readiness check so a hung dependency can't hang
// the probe itself.
const checkTimeout = 1 * time.Second

type namedCheck struct {
	name  string
	check func(ctx context.Context) error
}

// Checker holds the readiness checks and the draining flag.
// It is safe for concurrent use once all checks have been added.
type Checker struct {
	checks   []namedCheck
	draining atomic.Bool
}

// New returns a Checker with no checks.
func New() *Checker {
	return &Checker{}
}

// Add registers a readiness check. Call it during startup, before serving.
func (c *Checker) Add(name string, check func(ctx context.Context) error) {
	c.checks = append(c.checks, namedCheck{name: name, check: check})
}

// SetDraining marks the instance as shutting down: readiness fails from now
// on, liveness keeps passing.
func (c *Checker) SetDraining() {
	c.draining.Store(true)
}

type response struct {
	Status string   `json:"status"`
	Failed []string `json:"failed,omitempty"`
}

// LiveHandler always reports ok.
func (c *Checker) LiveHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, response{Status: "ok"})
	})
}

// ReadyHandler reports ok only when not draining and every check passes.
func (c *Checker) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.draining.Load() {
			writeJSON(w, http.StatusServiceUnavailable, response{Status: "draining"})
			return
		}
		if failed := c.runChecks(r.Context()); len(failed) > 0 {
			writeJSON(w, http.StatusServiceUnavailable, response{Status: "unavailable", Failed: failed})
			return
		}
		writeJSON(w, http.StatusOK, response{Status: "ok"})
	})
}

// runChecks runs all checks concurrently, each with its own timeout, and
// returns the names of the failed ones in registration order.
func (c *Checker) runChecks(ctx context.Context) []string {
	errs := make([]error, len(c.checks))

	var wg sync.WaitGroup
	for i, nc := range c.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, checkTimeout)
			defer cancel()
			errs[i] = nc.check(cctx)
		}()
	}
	wg.Wait()

	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, c.checks[i].name)
		}
	}
	return failed
}

func writeJSON(w http.ResponseWriter, status int, body response) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the client may be gone; nothing useful to do on error
}
