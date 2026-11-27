package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/Yulian302/deliverit/services/gateway/internal/auth"
)

// maxLoginBody bounds the login request; a real one is about 60 bytes.
const maxLoginBody = 4 << 10

// NewDemoLoginHandler serves POST /v1/auth/demo-login: {"userId": "<id>"}
// for one of the seeded demo users → a signed token. There are no passwords;
// this is a demo, and the point is to have real, verifiable identities.
func NewDemoLoginHandler(iss *auth.Issuer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UserID string `json:"userId"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF || req.UserID == "" {
			// Fixed text: never echo what the client sent.
			WriteProblem(w, http.StatusBadRequest, CodeInvalidArgument, `body must be {"userId": "<demo user id>"}`)
			return
		}
		u, ok := auth.FindUser(req.UserID)
		if !ok {
			WriteProblem(w, http.StatusNotFound, CodeNotFound, "no demo user with this id")
			return
		}
		token, expiresAt, err := iss.Issue(u)
		if err != nil {
			WriteProblem(w, http.StatusInternalServerError, CodeInternal, "could not issue a token")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store") // a token must not be cached
		_ = json.NewEncoder(w).Encode(loginResponse{
			Token:     token,
			TokenType: "Bearer",
			ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
			User: userJSON{
				ID:           u.ID,
				Name:         u.Name,
				Role:         string(u.Role),
				RestaurantID: u.RestaurantID,
			},
		})
	})
}

type loginResponse struct {
	Token     string   `json:"token"`
	TokenType string   `json:"tokenType"`
	ExpiresAt string   `json:"expiresAt"`
	User      userJSON `json:"user"`
}

type userJSON struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	Role         string `json:"role"`
	RestaurantID string `json:"restaurantId,omitempty"`
}
