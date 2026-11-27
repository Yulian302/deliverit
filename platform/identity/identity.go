// Package identity carries "who is calling" from the Gateway to the internal
// services.
//
// The Gateway is the only place that sees a JWT. Once it has verified one, it
// puts the caller's Identity in the request context, and Interceptor copies
// it into RPC headers on every outgoing call. On the receiving side the same
// interceptor reads the headers back into the context, so a handler just
// calls FromContext.
//
// The services trust these headers because internal RPC runs on a private
// network that only our own services can reach (SPEC §9.6; mTLS is the
// planned upgrade). The Gateway never copies headers from a public request
// onto an RPC, so a client can't set them itself.
package identity

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
)

// Role is what a user is allowed to do. The set is fixed by the spec.
type Role string

// The four roles (SPEC §9.6).
const (
	RoleCustomer   Role = "customer"
	RoleRestaurant Role = "restaurant"
	RoleCourier    Role = "courier"
	RoleOps        Role = "ops"
)

// ParseRole accepts exactly the four role names (case-sensitive).
func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case RoleCustomer, RoleRestaurant, RoleCourier, RoleOps:
		return r, nil
	}
	return "", fmt.Errorf("unknown role %q", s)
}

// Identity is the verified caller.
type Identity struct {
	UserID string
	Role   Role
	// RestaurantID is set only for restaurant staff: the restaurant they work for.
	RestaurantID string
}

// RPC headers that carry an Identity between services.
const (
	HeaderUserID       = "Deliverit-User-Id"
	HeaderRole         = "Deliverit-User-Role"
	HeaderRestaurantID = "Deliverit-Restaurant-Id"
)

type ctxKey struct{}

// NewContext returns ctx carrying id.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the caller's identity, if the request had one.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// Interceptor returns a Connect interceptor for both sides of a call:
//
//   - client: writes the context's Identity into the request headers (no
//     identity, no headers);
//   - server: reads the headers into the context. No user ID header means an
//     anonymous call, which public procedures accept. A user ID with an
//     invalid role is rejected as Unauthenticated: a trusted caller sent
//     something it shouldn't, so refuse rather than guess.
func Interceptor() connect.Interceptor {
	return interceptor{}
}

type interceptor struct{}

func (interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			write(ctx, req.Header())
			return next(ctx, req)
		}
		ctx, err := read(ctx, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		write(ctx, conn.RequestHeader())
		return conn
	}
}

func (interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := read(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

func write(ctx context.Context, h http.Header) {
	id, ok := FromContext(ctx)
	if !ok {
		return
	}
	h.Set(HeaderUserID, id.UserID)
	h.Set(HeaderRole, string(id.Role))
	if id.RestaurantID != "" {
		h.Set(HeaderRestaurantID, id.RestaurantID)
	}
}

func read(ctx context.Context, h http.Header) (context.Context, error) {
	userID := h.Get(HeaderUserID)
	if userID == "" {
		return ctx, nil
	}
	role, err := ParseRole(h.Get(HeaderRole))
	if err != nil {
		return ctx, connect.NewError(connect.CodeUnauthenticated, err)
	}
	return NewContext(ctx, Identity{
		UserID:       userID,
		Role:         role,
		RestaurantID: h.Get(HeaderRestaurantID),
	}), nil
}
