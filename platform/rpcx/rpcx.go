// Package rpcx holds Connect-RPC plumbing shared by every service, the RPC
// counterpart of httpx.
package rpcx

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
)

// AccessLog returns a server interceptor that writes one line per RPC:
// procedure, protocol, RPC status code and duration_ms.
//
// Why not httpx.AccessLog? An HTTP log only sees the HTTP status, and over
// gRPC a failed call is still HTTP 200 (the real status travels in a
// trailer). This logs the RPC code the caller actually gets.
//
// Install it AFTER the tracing interceptor:
//
//	connect.WithInterceptors(otelInterceptor, rpcx.AccessLog(log))
//
// Interceptors run in order, first = outermost, so it then runs inside the
// RPC's span, and the logger adds trace_id/span_id to the line.
//
// On clients it does nothing: the server side already logs every call.
func AccessLog(log *slog.Logger) connect.Interceptor {
	return &accessLog{log: log}
}

type accessLog struct {
	log *slog.Logger
}

func (a *accessLog) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		start := time.Now()
		resp, err := next(ctx, req)
		a.write(ctx, req.Spec(), req.Peer(), start, err)
		return resp, err
	}
}

func (a *accessLog) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := next(ctx, conn)
		a.write(ctx, conn.Spec(), conn.Peer(), start, err)
		return err
	}
}

func (a *accessLog) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // clients don't log; see AccessLog
}

func (a *accessLog) write(ctx context.Context, spec connect.Spec, peer connect.Peer, start time.Time, err error) {
	code := "ok"
	if err != nil {
		code = connect.CodeOf(err).String() // a non-Connect error becomes "unknown", as the client sees it
	}
	a.log.InfoContext(ctx, "rpc",
		slog.String("procedure", spec.Procedure),
		slog.String("protocol", peer.Protocol),
		slog.String("code", code),
		slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
	)
}
