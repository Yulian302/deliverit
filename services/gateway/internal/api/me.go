package api

import (
	"encoding/json"
	"net/http"

	"github.com/Yulian302/deliverit/platform/identity"
)

// NewMeHandler serves GET /v1/auth/me: who the token says you are. Mount it
// behind RequireRole(), so there is always an identity.
func NewMeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := identity.FromContext(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(userJSON{
			ID:           id.UserID,
			Role:         string(id.Role),
			RestaurantID: id.RestaurantID,
		})
	})
}
