// Package api is the Gateway's public REST API: request parsing, calls to
// internal services, and the JSON / problem+json responses clients see.
package api

import (
	"encoding/json"
	"net/http"
)

// Stable error codes, part of the public contract (api/openapi.yaml).
// Clients switch on these, never on the human-readable detail.
const (
	CodeInvalidArgument     = "invalid_argument"
	CodeUpstreamUnavailable = "upstream_unavailable"
	CodeUpstreamTimeout     = "upstream_timeout"
	CodeUpstreamError       = "upstream_error"
)

// problem is an RFC 9457 "problem details" body, plus our stable code.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// WriteProblem writes an RFC 9457 error response:
//
//	HTTP/1.1 400 Bad Request
//	Content-Type: application/problem+json
//
//	{"type":"about:blank","title":"Bad Request","status":400,
//	 "code":"invalid_argument","detail":"near is required, as near=lat,lng"}
//
// type "about:blank" means "the HTTP status says it all"; code is the
// machine-readable reason; detail is for humans and must never contain
// internal information (stack traces, SQL, hostnames).
func WriteProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Code:   code,
		Detail: detail,
	})
}
