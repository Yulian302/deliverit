// Package logging builds the structured JSON logger every DeliverIt service uses.
//
// The logger is a plain *slog.Logger. What it adds: whenever you log with a
// context (log.InfoContext(ctx, ...)), it copies correlation IDs out of the
// context into the record —
//
//   - trace_id, span_id: from the active OpenTelemetry span, so a log line
//     can be joined to its trace in Grafana;
//   - request_id: set by the HTTP middleware via WithRequestID.
//
// Callers never add these by hand; passing ctx is enough.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"
)

// New returns a JSON logger writing to w. Every record carries "service".
// level is one of debug, info, warn, error (case-insensitive); anything else
// is an error, so a typo in LOG_LEVEL fails at startup instead of silently
// logging at the wrong level.
func New(w io.Writer, service, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q (want debug|info|warn|error): %w", level, err)
	}

	// Every record goes two ways:
	//   - JSON on w (stdout): what `docker logs` and a terminal show;
	//   - the OpenTelemetry bridge: exported over OTLP to Loki, carrying the
	//     trace context of the span in ctx. Until telemetry.Setup installs a
	//     real LoggerProvider, the global one discards, so creating the logger
	//     first (as mains do) is fine.
	jsonOut := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl}).
		WithAttrs([]slog.Attr{slog.String("service", service)})
	toOTel := &levelGate{min: lvl, next: otelslog.NewHandler(instrumentationName)}
	base := slog.NewMultiHandler(jsonOut, toOTel)

	return slog.New(&contextHandler{base: base, built: base}), nil
}

// instrumentationName identifies this logger in exported records.
const instrumentationName = "github.com/Yulian302/deliverit/platform/logging"

// levelGate drops records below min before they reach next. The otelslog
// handler doesn't know LOG_LEVEL; without this, debug lines hidden on stdout
// would still be shipped to (and stored in) Loki.
type levelGate struct {
	min  slog.Level
	next slog.Handler
}

func (g *levelGate) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= g.min && g.next.Enabled(ctx, l)
}

func (g *levelGate) Handle(ctx context.Context, r slog.Record) error {
	return g.next.Handle(ctx, r)
}

func (g *levelGate) WithAttrs(as []slog.Attr) slog.Handler {
	return &levelGate{min: g.min, next: g.next.WithAttrs(as)}
}

func (g *levelGate) WithGroup(name string) slog.Handler {
	return &levelGate{min: g.min, next: g.next.WithGroup(name)}
}

type requestIDKey struct{}

// WithRequestID returns a copy of ctx carrying the request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request ID stored in ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// contextHandler wraps a slog.Handler and adds correlation IDs from the
// context to every record, always at the top level of the JSON object.
//
// Why it keeps base and ops separately: slog nests every attribute added after
// WithGroup("x") under "x". If we just added trace_id to the record, a logger
// created with .WithGroup("req") would write {"req":{"trace_id":...}} and
// Grafana could no longer find it. So when groups are in play, we rebuild the
// chain per record: base + correlation attrs first, then the caller's
// With/WithGroup calls on top.
type contextHandler struct {
	base    slog.Handler                      // JSON handler + service attr, no caller groups
	ops     []func(slog.Handler) slog.Handler // caller's WithAttrs/WithGroup calls, in order
	built   slog.Handler                      // base with ops applied (fast path)
	grouped bool                              // whether any op is a WithGroup
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.built.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := correlationAttrs(ctx)
	if len(attrs) == 0 {
		return h.built.Handle(ctx, r)
	}

	if !h.grouped {
		// Fast path: no groups, so record attrs land at the top level anyway.
		r = r.Clone() // the record may be shared; don't mutate the caller's copy
		r.AddAttrs(attrs...)
		return h.built.Handle(ctx, r)
	}

	// Slow path (only for loggers that used WithGroup): rebuild the chain so
	// the correlation attrs sit outside every group.
	hh := h.base.WithAttrs(attrs)
	for _, op := range h.ops {
		hh = op(hh)
	}
	return hh.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	return h.with(func(x slog.Handler) slog.Handler { return x.WithAttrs(as) }, false)
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(func(x slog.Handler) slog.Handler { return x.WithGroup(name) }, true)
}

// with returns a new handler with op appended; it never modifies h, because
// slog handlers must be immutable once shared.
func (h *contextHandler) with(op func(slog.Handler) slog.Handler, isGroup bool) *contextHandler {
	ops := make([]func(slog.Handler) slog.Handler, len(h.ops), len(h.ops)+1)
	copy(ops, h.ops)
	return &contextHandler{
		base:    h.base,
		ops:     append(ops, op),
		built:   op(h.built),
		grouped: h.grouped || isGroup,
	}
}

// correlationAttrs extracts trace/span/request IDs from ctx (only those present).
func correlationAttrs(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs,
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if id := RequestIDFrom(ctx); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	return attrs
}
