package auth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Yulian302/deliverit/platform/identity"
	"github.com/Yulian302/deliverit/services/gateway/internal/auth"
)

var t0 = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

const (
	khreshchatykStaff = "0193a1b1-0000-7000-8000-0000000000a1"
	khreshchatyk      = "0193a1b0-0000-7000-8000-000000000001"
	podil             = "0193a1b0-0000-7000-8000-000000000005"
	olena             = "0193a1b1-0000-7000-8000-0000000000c1"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func at(ts time.Time) func() time.Time { return func() time.Time { return ts } }

func mustUser(t *testing.T, id string) auth.User {
	t.Helper()
	u, ok := auth.FindUser(id)
	if !ok {
		t.Fatalf("demo user %s missing", id)
	}
	return u
}

func TestParseSigningKey(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	k, err := auth.ParseSigningKey(base64.StdEncoding.EncodeToString(seed))
	if err != nil {
		t.Fatal(err)
	}
	if !k.Equal(ed25519.NewKeyFromSeed(seed)) {
		t.Fatal("key does not match its seed")
	}

	for name, in := range map[string]string{
		"16 bytes":   base64.StdEncoding.EncodeToString(seed[:16]),
		"not base64": "%%%",
		"empty":      "",
	} {
		if _, err := auth.ParseSigningKey(in); err == nil || !strings.Contains(err.Error(), "JWT_SIGNING_KEY") {
			t.Errorf("%s: err = %v; want an error naming JWT_SIGNING_KEY", name, err)
		}
	}
}

func TestGenerateSigningKey(t *testing.T) {
	a, err := auth.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := auth.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two generated keys are equal")
	}
	if _, err := auth.ParseSigningKey(a); err != nil {
		t.Fatalf("generated key doesn't parse: %v", err)
	}
}

func TestIssueVerify_RoundTrip(t *testing.T) {
	iss := auth.NewIssuer(newKey(t), time.Hour, at(t0))
	tok, exp, err := iss.Issue(mustUser(t, khreshchatykStaff))
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(t0.Add(time.Hour)) {
		t.Errorf("expiresAt = %v; want %v", exp, t0.Add(time.Hour))
	}
	got, err := iss.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	want := identity.Identity{UserID: khreshchatykStaff, Role: identity.RoleRestaurant, RestaurantID: khreshchatyk}
	if got != want {
		t.Fatalf("Verify = %+v; want %+v", got, want)
	}
}

// claims builds a valid claim set for c1 at t0, for tests to break one field.
func claims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":  "deliverit-gateway",
		"aud":  []string{"deliverit"},
		"sub":  olena,
		"role": "customer",
		"iat":  t0.Unix(),
		"exp":  t0.Add(time.Hour).Unix(),
		"jti":  uuid.NewString(),
	}
}

func sign(t *testing.T, key ed25519.PrivateKey, c jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerify_Rejects(t *testing.T) {
	key := newKey(t)
	iss := auth.NewIssuer(key, time.Hour, at(t0))
	valid := sign(t, key, claims())

	with := func(k string, v any) jwt.MapClaims { c := claims(); c[k] = v; return c }
	without := func(k string) jwt.MapClaims { c := claims(); delete(c, k); return c }

	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims()).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	hs256, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims()).SignedString([]byte(key.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, ".")
	other := claims()
	other["role"] = "ops"
	otherParts := strings.Split(sign(t, key, other), ".")
	tampered := parts[0] + "." + otherParts[1] + "." + parts[2]

	cases := map[string]struct {
		token string
		now   time.Time
	}{
		"expired 31s ago":         {valid, t0.Add(time.Hour + 31*time.Second)},
		"another key":             {sign(t, newKey(t), claims()), t0},
		"alg none":                {none, t0},
		"HS256 with public key":   {hs256, t0},
		"wrong issuer":            {sign(t, key, with("iss", "other")), t0},
		"wrong audience":          {sign(t, key, with("aud", []string{"other"})), t0},
		"no exp":                  {sign(t, key, without("exp")), t0},
		"tampered payload":        {tampered, t0},
		"not a jwt":               {"not.a.jwt", t0},
		"unknown role":            {sign(t, key, with("role", "admin")), t0},
		"restaurant role, no rid": {sign(t, key, with("role", "restaurant")), t0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v := auth.NewIssuer(key, time.Hour, at(tc.now))
			if _, err := v.Verify(tc.token); !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("err = %v; want ErrInvalidToken", err)
			}
		})
	}
	// Sanity: the untouched token is fine, so each case fails for its own reason.
	if _, err := iss.Verify(valid); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func TestVerify_LeewayAcceptsSmallSkew(t *testing.T) {
	key := newKey(t)
	tok, _, err := auth.NewIssuer(key, time.Hour, at(t0)).Issue(mustUser(t, olena))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.NewIssuer(key, time.Hour, at(t0.Add(time.Hour+10*time.Second))).Verify(tok); err != nil {
		t.Fatalf("10s past expiry should pass with 30s leeway: %v", err)
	}
}

func TestDemoUsers(t *testing.T) {
	seen := map[string]bool{}
	roles := map[identity.Role]bool{}
	for _, u := range auth.DemoUsers() {
		if seen[u.ID] {
			t.Errorf("duplicate id %s", u.ID)
		}
		seen[u.ID] = true
		if _, err := uuid.Parse(u.ID); err != nil {
			t.Errorf("id %q is not a UUID", u.ID)
		}
		roles[u.Role] = true
		if u.Role == identity.RoleRestaurant {
			if u.RestaurantID != khreshchatyk && u.RestaurantID != podil {
				t.Errorf("%s: restaurant %q is not a seeded restaurant", u.Name, u.RestaurantID)
			}
		} else if u.RestaurantID != "" {
			t.Errorf("%s: non-restaurant user has a restaurant", u.Name)
		}
		if got, ok := auth.FindUser(u.ID); !ok || got != u {
			t.Errorf("FindUser(%s) = %+v, %v", u.ID, got, ok)
		}
	}
	for _, r := range []identity.Role{identity.RoleCustomer, identity.RoleRestaurant, identity.RoleCourier, identity.RoleOps} {
		if !roles[r] {
			t.Errorf("no demo user with role %s", r)
		}
	}
	if _, ok := auth.FindUser(uuid.NewString()); ok {
		t.Error("FindUser found an unknown id")
	}
}
