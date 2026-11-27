// Package auth is the Gateway's demo authentication: the seeded demo users,
// the Ed25519 signing key, and issuing and verifying JWTs.
//
// Only the Gateway uses it. Internal services never see a token; they get
// the verified caller from platform/identity.
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// ParseSigningKey decodes JWT_SIGNING_KEY: standard base64 of a 32-byte
// Ed25519 seed. Errors name the variable, because that's what the operator
// has to fix, and never include its value.
func ParseSigningKey(b64 string) (ed25519.PrivateKey, error) {
	if b64 == "" {
		return nil, errors.New("JWT_SIGNING_KEY is empty (generate one with: gateway keygen)")
	}
	seed, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("JWT_SIGNING_KEY is not valid base64 (generate one with: gateway keygen)")
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("JWT_SIGNING_KEY must decode to %d bytes, got %d (generate one with: gateway keygen)", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// GenerateSigningKey returns a new random key in JWT_SIGNING_KEY format.
func GenerateSigningKey() (string, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(seed), nil
}
