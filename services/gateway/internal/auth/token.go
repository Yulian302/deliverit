package auth

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Yulian302/deliverit/platform/identity"
)

const (
	issuer   = "deliverit-gateway"
	audience = "deliverit"
	// leeway tolerates small clock differences between machines.
	leeway = 30 * time.Second
)

// ErrInvalidToken wraps every reason a token is refused. Callers answer 401
// and don't tell the client which check failed.
var ErrInvalidToken = errors.New("invalid token")

// claims is the token body: the registered claims plus our two.
type claims struct {
	jwt.RegisteredClaims
	Role string `json:"role"`
	// RestaurantID ("rid") is present only for restaurant staff.
	RestaurantID string `json:"rid,omitempty"`
}

// Issuer signs and verifies the Gateway's tokens.
type Issuer struct {
	key    ed25519.PrivateKey
	ttl    time.Duration
	now    func() time.Time
	parser *jwt.Parser
}

// NewIssuer returns an Issuer whose tokens live for ttl. now is the clock
// (time.Now in production, fixed in tests).
func NewIssuer(key ed25519.PrivateKey, ttl time.Duration, now func() time.Time) *Issuer {
	return &Issuer{
		key: key,
		ttl: ttl,
		now: now,
		parser: jwt.NewParser(
			// Only EdDSA: this is what stops "alg: none" and the HS256
			// trick of signing with the public key as an HMAC secret.
			jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
			jwt.WithIssuer(issuer),
			jwt.WithAudience(audience),
			jwt.WithExpirationRequired(),
			jwt.WithLeeway(leeway),
			jwt.WithTimeFunc(now),
		),
	}
}

// Issue signs a token for u and returns it with its expiry time.
func (i *Issuer) Issue(u User) (token string, expiresAt time.Time, err error) {
	now := i.now()
	expiresAt = now.Add(i.ttl)
	jti, err := uuid.NewV7()
	if err != nil {
		return "", time.Time{}, err
	}
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{audience},
			Subject:   u.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        jti.String(),
		},
		Role:         string(u.Role),
		RestaurantID: u.RestaurantID,
	}
	token, err = jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(i.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return token, expiresAt, nil
}

// Verify checks the signature, algorithm, issuer, audience and expiry, then
// returns who the token is for. Every failure wraps ErrInvalidToken.
func (i *Issuer) Verify(token string) (identity.Identity, error) {
	var c claims
	_, err := i.parser.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) {
		return i.key.Public(), nil
	})
	if err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if c.Subject == "" {
		return identity.Identity{}, fmt.Errorf("%w: no subject", ErrInvalidToken)
	}
	role, err := identity.ParseRole(c.Role)
	if err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if role == identity.RoleRestaurant && c.RestaurantID == "" {
		return identity.Identity{}, fmt.Errorf("%w: restaurant role without rid", ErrInvalidToken)
	}
	return identity.Identity{UserID: c.Subject, Role: role, RestaurantID: c.RestaurantID}, nil
}
