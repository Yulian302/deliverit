package api

import (
	"net/http"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Yulian302/deliverit/platform/identity"
	"github.com/Yulian302/deliverit/services/gateway/internal/auth"
)

// Authenticate verifies the request's bearer token, if it has one, and puts
// the caller's identity in the request context.
//
//   - No Authorization header: the request continues anonymously. Public
//     routes work; RequireRole turns it away from the others.
//   - "Bearer <valid token>" (scheme in any case, per RFC 9110): continues
//     with the identity, and the span records who called.
//   - Anything else: 401. A bad token is refused even on a public route, so
//     a client finds out its token is broken instead of silently becoming
//     anonymous.
func Authenticate(iss *auth.Issuer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if h == "" {
				next.ServeHTTP(w, r)
				return
			}
			scheme, token, found := strings.Cut(h, " ")
			if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
				unauthenticated(w, "expected Authorization: Bearer <token>")
				return
			}
			id, err := iss.Verify(token)
			if err != nil {
				// The reason stays server-side: telling a client which check
				// failed only helps someone forging tokens.
				unauthenticated(w, "token is invalid or expired")
				return
			}
			trace.SpanFromContext(r.Context()).SetAttributes(
				attribute.String("enduser.id", id.UserID),
				attribute.String("enduser.role", string(id.Role)),
			)
			next.ServeHTTP(w, r.WithContext(identity.NewContext(r.Context(), id)))
		})
	}
}

// RequireRole lets a request through only if Authenticate found a caller
// with one of roles. With no roles, any signed-in caller passes.
// Anonymous → 401; signed in with another role → 403.
func RequireRole(roles ...identity.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := identity.FromContext(r.Context())
			if !ok {
				unauthenticated(w, "sign in first: POST /v1/auth/demo-login")
				return
			}
			if len(roles) > 0 && !slices.Contains(roles, id.Role) {
				WriteProblem(w, http.StatusForbidden, CodeForbidden, "your role can't do this")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unauthenticated(w http.ResponseWriter, detail string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="deliverit"`)
	WriteProblem(w, http.StatusUnauthorized, CodeUnauthenticated, detail)
}
