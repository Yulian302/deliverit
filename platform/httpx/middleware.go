package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/google/uuid"

	"github.com/Yulian302/deliverit/platform/logging"
)

// HeaderRequestID is the header that carries the request ID in and out.
const HeaderRequestID = "X-Request-ID"

// validRequestID is what we accept from clients. Anything else (too long,
// spaces, newlines, quotes...) could pollute logs or be used for log
// injection, so it is replaced instead of echoed.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Chain wraps h in mws. The first middleware in the list is the outermost,
// so Chain(h, A, B) runs A → B → h.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// RequestID gives every request an ID: the client's X-Request-ID if it is
// well-formed, otherwise a fresh UUIDv7 (time-ordered, so IDs sort by when
// the request arrived). The ID goes into the context, where the logger picks
// it up, and back to the client in the response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

func newRequestID() string {
	id, err := uuid.NewV7()
	if err != nil { // only if the OS random source fails
		return uuid.NewString()
	}
	return id.String()
}

// Recover turns a panic in a handler into a 500 problem+json response and
// logs it with the stack, so one bad request can't take the process down.
// The panic value is logged, never sent to the client.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					panic(rec) // deliberate abort: let net/http close the connection quietly
				}
				log.ErrorContext(r.Context(), "panic in handler",
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)
				writeInternalProblem(w)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// writeInternalProblem writes the RFC 9457 body for an unexpected failure.
// Gateway's api.WriteProblem (Task 9) produces the same shape for its own
// errors; this copy exists because platform/ must not import services/.
func writeInternalProblem(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(http.StatusInternalServerError),
		"status": http.StatusInternalServerError,
		"code":   "internal",
	})
}

// Deadline gives each request context a timeout of d. Downstream calls
// (RPC, DB) that take the context stop when it expires.
func Deadline(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AccessLog writes one line per request: method, path, status, duration_ms.
// It logs with the request context, so request_id and trace_id are attached
// when the middleware that sets them runs before this one.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			log.InfoContext(r.Context(), "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			)
		})
	}
}

// statusWriter remembers the status code a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer (for Flush,
// deadlines, etc.) through this wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
