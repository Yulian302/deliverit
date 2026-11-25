// Package httpx holds the HTTP plumbing every DeliverIt service shares: a
// server with safe timeouts and the standard middleware (request ID, panic
// recovery, request deadline, access log).
package httpx

import (
	"net/http"
	"time"
)

// NewServer returns an *http.Server with the timeouts from the spec.
//
//   - ReadHeaderTimeout stops slow-loris clients from holding a connection
//     open by trickling headers.
//   - IdleTimeout closes keep-alive connections nobody is using.
//
// There is no WriteTimeout on purpose: it would also cut off legitimately
// slow responses. Per-request time limits come from the Deadline middleware,
// which cancels the request context instead of killing the connection.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
