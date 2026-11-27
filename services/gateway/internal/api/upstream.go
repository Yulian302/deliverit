package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"connectrpc.com/connect"
)

// writeUpstreamError turns a failed call to Order into the response the
// public sees. Only INVALID_ARGUMENT passes Order's message through: Order
// wrote it for the caller ("line 2: quantity must be 1-20"). Every other
// message stays in our logs.
func writeUpstreamError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	ctx := r.Context()
	code := connect.CodeOf(err)
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		// The client hung up: nobody is listening for a response.
		return
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		// Our own 5s request budget ran out, wherever the call was.
		WriteProblem(w, http.StatusGatewayTimeout, CodeUpstreamTimeout, "the order service did not answer in time")
	case code == connect.CodeUnavailable, isDialFailure(err):
		// Checked before DeadlineExceeded: Connect reports a *connect*
		// timeout (host dropping packets, cut short by our dial timeout) as
		// DeadlineExceeded, but that means "couldn't reach Order", not "Order
		// was too slow".
		log.WarnContext(ctx, "order unavailable", slog.Any("error", err))
		WriteProblem(w, http.StatusServiceUnavailable, CodeUpstreamUnavailable, "the order service is unavailable, try again later")
	case code == connect.CodeDeadlineExceeded:
		WriteProblem(w, http.StatusGatewayTimeout, CodeUpstreamTimeout, "the order service did not answer in time")
	case code == connect.CodeInvalidArgument:
		WriteProblem(w, http.StatusBadRequest, CodeInvalidArgument, upstreamMessage(err))
	case code == connect.CodeNotFound:
		WriteProblem(w, http.StatusNotFound, CodeNotFound, "not found")
	case code == connect.CodeAlreadyExists:
		WriteProblem(w, http.StatusUnprocessableEntity, CodeIdempotencyKeyReused,
			"this Idempotency-Key was already used for a different request; use a new key")
	case code == connect.CodeUnauthenticated:
		// The Gateway checks first, so this means Order and Gateway disagree.
		log.WarnContext(ctx, "order says unauthenticated", slog.Any("error", err))
		unauthenticated(w, "sign in first: POST /v1/auth/demo-login")
	case code == connect.CodePermissionDenied:
		WriteProblem(w, http.StatusForbidden, CodeForbidden, "your role can't do this")
	default:
		log.ErrorContext(ctx, "order call failed", slog.Any("error", err))
		WriteProblem(w, http.StatusBadGateway, CodeUpstreamError, "the order service failed")
	}
}

// isDialFailure reports whether err comes from failing to open a connection
// (refused, unreachable, or connect timed out). Connect keeps the network
// error underneath its own, so errors.As can find it.
func isDialFailure(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// upstreamMessage is the message of a Connect error without its
// "invalid_argument: " code prefix.
func upstreamMessage(err error) string {
	if ce := new(connect.Error); errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}
