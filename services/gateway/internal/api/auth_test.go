package api_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Yulian302/deliverit/platform/identity"
	"github.com/Yulian302/deliverit/services/gateway/internal/api"
	"github.com/Yulian302/deliverit/services/gateway/internal/auth"
)

const (
	olena             = "0193a1b1-0000-7000-8000-0000000000c1"
	khreshchatykStaff = "0193a1b1-0000-7000-8000-0000000000a1"
	khreshchatyk      = "0193a1b0-0000-7000-8000-000000000001"
	courierAndrii     = "0193a1b1-0000-7000-8000-0000000000b1"
	opsDesk           = "0193a1b1-0000-7000-8000-0000000000d1"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func issuerAt(key ed25519.PrivateKey, now time.Time) *auth.Issuer {
	return auth.NewIssuer(key, time.Hour, func() time.Time { return now })
}

func newIssuer(t *testing.T, now time.Time) *auth.Issuer {
	t.Helper()
	return issuerAt(newKey(t), now)
}

func tokenFor(t *testing.T, iss *auth.Issuer, userID string) string {
	t.Helper()
	u, ok := auth.FindUser(userID)
	if !ok {
		t.Fatalf("no demo user %s", userID)
	}
	tok, _, err := iss.Issue(u)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func decodeProblemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q; want application/problem+json", ct)
	}
	var p struct{ Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p.Code
}

func login(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/demo-login", strings.NewReader(body)))
	return rec
}

func TestDemoLogin_OK(t *testing.T) {
	iss := newIssuer(t, time.Now())
	rec := login(api.NewDemoLoginHandler(iss), `{"userId":"`+olena+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		Token     string
		TokenType string
		ExpiresAt time.Time
		User      map[string]any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.TokenType != "Bearer" {
		t.Errorf("tokenType = %q", out.TokenType)
	}
	id, err := iss.Verify(out.Token)
	if err != nil || id.UserID != olena || id.Role != identity.RoleCustomer {
		t.Errorf("token verifies to %+v, %v", id, err)
	}
	if out.User["role"] != "customer" || out.User["id"] != olena || out.User["name"] != "Olena" {
		t.Errorf("user = %v", out.User)
	}
	if _, has := out.User["restaurantId"]; has {
		t.Error("customer has a restaurantId key")
	}
	if out.ExpiresAt.IsZero() {
		t.Error("expiresAt missing")
	}
}

func TestDemoLogin_BadRequests(t *testing.T) {
	h := api.NewDemoLoginHandler(newIssuer(t, time.Now()))
	for name, body := range map[string]string{
		"not json":      `not json`,
		"empty body":    ``,
		"empty object":  `{}`,
		"empty userId":  `{"userId":""}`,
		"unknown field": `{"userId":"` + olena + `","extra":1}`,
		"5 KiB body":    `{"userId":"` + strings.Repeat("x", 5<<10) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := login(h, body)
			if rec.Code != http.StatusBadRequest || decodeProblemCode(t, rec) != api.CodeInvalidArgument {
				t.Fatalf("status = %d, body %s; want 400 invalid_argument", rec.Code, rec.Body)
			}
		})
	}
	rec := login(h, `{"userId":"0193a1b1-0000-7000-8000-0000000000ff"}`)
	if rec.Code != http.StatusNotFound || decodeProblemCode(t, rec) != api.CodeNotFound {
		t.Fatalf("unknown user: status = %d, body %s; want 404 not_found", rec.Code, rec.Body)
	}
}

func TestAuthenticate(t *testing.T) {
	now := time.Now()
	key := newKey(t)
	iss := issuerAt(key, now)
	valid := tokenFor(t, iss, olena)
	// Same key, issued 2h ago with a 1h TTL: expired an hour ago.
	expired := tokenFor(t, issuerAt(key, now.Add(-2*time.Hour)), olena)

	var seen *identity.Identity
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = nil
		if id, ok := identity.FromContext(r.Context()); ok {
			seen = &id
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h := api.Authenticate(iss)(next)

	ok := []struct {
		name, header string
		wantID       bool
	}{
		{"no header", "", false},
		{"Bearer valid", "Bearer " + valid, true},
		{"lowercase bearer", "bearer " + valid, true},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			seen = nil
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d", rec.Code)
			}
			if (seen != nil) != tc.wantID || (seen != nil && seen.UserID != olena) {
				t.Fatalf("identity = %+v; want present=%v", seen, tc.wantID)
			}
		})
	}

	for name, header := range map[string]string{
		"basic":       "Basic dXNlcjpwYXNz",
		"bearer only": "Bearer",
		"expired":     "Bearer " + expired,
		"garbage":     "Bearer garbage",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", header)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || decodeProblemCode(t, rec) != api.CodeUnauthenticated {
				t.Fatalf("status = %d, body %s; want 401 unauthenticated", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="deliverit"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			if tok := strings.TrimPrefix(header, "Bearer "); len(tok) > 8 && strings.Contains(rec.Body.String(), tok) {
				t.Error("response echoes the token")
			}
		})
	}
}

func serveAs(t *testing.T, h http.Handler, iss *auth.Issuer, userID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if userID != "" {
		req.Header.Set("Authorization", "Bearer "+tokenFor(t, iss, userID))
	}
	rec := httptest.NewRecorder()
	api.Authenticate(iss)(h).ServeHTTP(rec, req)
	return rec
}

func TestRequireRole(t *testing.T) {
	iss := newIssuer(t, time.Now())
	okH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	opsOnly := api.RequireRole(identity.RoleOps)(okH)
	anyone := api.RequireRole()(okH)

	if rec := serveAs(t, opsOnly, iss, ""); rec.Code != http.StatusUnauthorized || decodeProblemCode(t, rec) != api.CodeUnauthenticated {
		t.Errorf("anonymous: status = %d; want 401", rec.Code)
	}
	if rec := serveAs(t, opsOnly, iss, olena); rec.Code != http.StatusForbidden || decodeProblemCode(t, rec) != api.CodeForbidden {
		t.Errorf("customer on ops route: status = %d; want 403", rec.Code)
	}
	if rec := serveAs(t, opsOnly, iss, opsDesk); rec.Code != http.StatusOK {
		t.Errorf("ops: status = %d; want 200", rec.Code)
	}
	if rec := serveAs(t, anyone, iss, courierAndrii); rec.Code != http.StatusOK {
		t.Errorf("courier on any-role route: status = %d; want 200", rec.Code)
	}
}

func TestMe(t *testing.T) {
	iss := newIssuer(t, time.Now())
	rec := serveAs(t, api.RequireRole()(api.NewMeHandler()), iss, khreshchatykStaff)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"id": khreshchatykStaff, "role": "restaurant", "restaurantId": khreshchatyk}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q; want %q", k, got[k], v)
		}
	}
}
