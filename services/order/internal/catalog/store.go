package catalog

import (
	"context"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yulian302/deliverit/services/order/internal/catalog/dbgen"
)

// earthRadiusKm is the mean Earth radius; the SQL distance formula uses the
// same value, so the box and the exact distance agree.
const earthRadiusKm = 6371.0

// PostgresStore implements Store on Postgres through sqlc-generated queries.
type PostgresStore struct {
	q *dbgen.Queries
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore returns a store that queries through pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{q: dbgen.New(pool)}
}

// ListNear implements Store.
func (s *PostgresStore) ListNear(ctx context.Context, q NearbyQuery) ([]Restaurant, error) {
	b := boundingBox(q.Center, q.RadiusKm)
	rows, err := s.q.ListRestaurantsNear(ctx, dbgen.ListRestaurantsNearParams{
		Lat:      q.Center.Lat,
		Lng:      q.Center.Lng,
		MinLat:   b.MinLat,
		MaxLat:   b.MaxLat,
		MinLng:   b.MinLng,
		MaxLng:   b.MaxLng,
		RadiusKm: q.RadiusKm,
		Lim:      int32(q.Limit), // validated: 1..MaxLimit
	})
	if err != nil {
		return nil, fmt.Errorf("catalog store: list near: %w", err)
	}

	out := make([]Restaurant, 0, len(rows)) // non-nil even when empty (Store contract)
	for _, r := range rows {
		out = append(out, Restaurant{
			ID:          r.ID,
			Name:        r.Name,
			Location:    Point{Lat: r.Lat, Lng: r.Lng},
			PrepMinutes: int(r.PrepMinutes),
			DistanceKm:  r.DistanceKm,
		})
	}
	return out, nil
}

// box is a latitude/longitude rectangle, in degrees.
type box struct {
	MinLat, MaxLat, MinLng, MaxLng float64
}

// boundingBox returns the smallest lat/lng rectangle that contains every
// point within radiusKm of c. The query uses it as a cheap, indexable
// pre-filter before computing exact distances, so it must never be too
// small: a point it excludes is a restaurant silently missing from results.
//
// Method (J. P. Matuschek, "Finding Points Within a Distance of a
// Latitude/Longitude Using Bounding Coordinates"): with the angular radius
// d = radiusKm / R, latitude extends exactly ±d; longitude extends
// ±asin(sin d / cos lat), which widens as you move away from the equator.
//
// Two cases can't be one rectangle, so longitude is left unrestricted:
//   - the circle contains a pole (every longitude meets there);
//   - the circle crosses the ±180° meridian (the range would wrap around).
func boundingBox(c Point, radiusKm float64) box {
	const toRad, toDeg = math.Pi / 180, 180 / math.Pi
	d := radiusKm / earthRadiusKm // angular radius, radians
	lat, lng := c.Lat*toRad, c.Lng*toRad

	minLat, maxLat := lat-d, lat+d
	minLng, maxLng := -math.Pi, math.Pi

	if minLat > -math.Pi/2 && maxLat < math.Pi/2 {
		dLng := math.Asin(math.Sin(d) / math.Cos(lat))
		minLng, maxLng = lng-dLng, lng+dLng
		if minLng < -math.Pi || maxLng > math.Pi {
			minLng, maxLng = -math.Pi, math.Pi // crosses ±180°
		}
	} else {
		// A pole is inside the circle: clamp latitude, keep all longitudes.
		minLat, maxLat = math.Max(minLat, -math.Pi/2), math.Min(maxLat, math.Pi/2)
	}

	return box{
		MinLat: minLat * toDeg, MaxLat: maxLat * toDeg,
		MinLng: minLng * toDeg, MaxLng: maxLng * toDeg,
	}
}
