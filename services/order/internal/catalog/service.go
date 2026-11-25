package catalog

import (
	"context"
	"fmt"
)

// Search limits. Zero in a query means the default.
const (
	DefaultRadiusKm = 5.0
	MaxRadiusKm     = 20.0
	DefaultLimit    = 20
	MaxLimit        = 50
)

// Store finds restaurants. Implementations must keep these promises, which
// Service and its callers rely on (Go's type system can't express them):
//
//   - only restaurants within q.RadiusKm of q.Center (great-circle distance);
//   - ordered nearest first, ties broken by ID, so results are stable;
//   - at most q.Limit results;
//   - DistanceKm filled in for each result;
//   - an empty, non-nil slice when nothing matches.
//
// q is always already validated and has its defaults applied.
type Store interface {
	ListNear(ctx context.Context, q NearbyQuery) ([]Restaurant, error)
}

// Service holds the catalog's business rules. It knows nothing about HTTP,
// RPC or SQL; it talks to storage only through the Store interface.
type Service struct {
	store Store
}

// NewService returns a Service that reads from s.
func NewService(s Store) *Service {
	return &Service{store: s}
}

// ListNearby validates q, fills in defaults, and returns matching
// restaurants nearest first. Bad input returns an error wrapping
// ErrInvalidArgument, and the store is not called.
func (s *Service) ListNearby(ctx context.Context, q NearbyQuery) ([]Restaurant, error) {
	q, err := normalize(q)
	if err != nil {
		return nil, err
	}
	rs, err := s.store.ListNear(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list nearby: %w", err)
	}
	return rs, nil
}

// normalize checks q and applies defaults.
//
// Float checks go through inRange so NaN and ±Inf are rejected too (both can
// arrive in a protobuf double).
func normalize(q NearbyQuery) (NearbyQuery, error) {
	if !inRange(q.Center.Lat, -90, 90) {
		return q, invalid("lat %v must be within [-90, 90]", q.Center.Lat)
	}
	if !inRange(q.Center.Lng, -180, 180) {
		return q, invalid("lng %v must be within [-180, 180]", q.Center.Lng)
	}

	switch {
	case q.RadiusKm == 0:
		q.RadiusKm = DefaultRadiusKm
	case !inRange(q.RadiusKm, 0, MaxRadiusKm):
		return q, invalid("radius_km %v must be within (0, %v]", q.RadiusKm, MaxRadiusKm)
	}

	switch {
	case q.Limit == 0:
		q.Limit = DefaultLimit
	case q.Limit < 0 || q.Limit > MaxLimit:
		return q, invalid("limit %d must be within (0, %d]", q.Limit, MaxLimit)
	}
	return q, nil
}

// inRange reports whether minV <= v <= maxV; false for NaN and ±Inf.
//
// Keep it written as "is inside", never as "is not outside": every comparison
// with NaN is false, so `v >= minV && v <= maxV` rejects NaN by itself, while
// the tempting `!(v < minV || v > maxV)` would accept it. ±Inf is simply
// outside any finite range.
func inRange(v, minV, maxV float64) bool {
	return v >= minV && v <= maxV
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidArgument}, args...)...)
}
