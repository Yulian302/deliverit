// Package catalog is Order's restaurant catalog: finding restaurants near a
// point, and reading a restaurant's menu.
//
// Layers, one file each (enforced by depguard in .golangci.yml):
//
//	rpc.go      transport: protobuf ⇄ model, errors → Connect codes
//	service.go  business rules: validation and defaults (no HTTP, RPC or DB)
//	store.go    storage: SQL through sqlc-generated code in dbgen/
//	model.go    the types all layers share
package catalog

import (
	"errors"

	"github.com/google/uuid"
)

// Point is a location in degrees (WGS84, like GPS).
type Point struct {
	Lat, Lng float64
}

// Restaurant is a restaurant as returned by a nearby search.
type Restaurant struct {
	ID          uuid.UUID
	Name        string
	Location    Point
	PrepMinutes int
	// DistanceKm is the great-circle distance from the searched point.
	DistanceKm float64
}

// NearbyQuery asks for restaurants within RadiusKm of Center, at most Limit.
// Zero RadiusKm or Limit means "use the default" (see Service.ListNearby).
type NearbyQuery struct {
	Center   Point
	RadiusKm float64
	Limit    int
}

// Currency is the one currency the service works in (ISO 4217). Prices are
// integer minor units of it (kopecks).
const Currency = "UAH"

// MenuItem is something a restaurant sells.
type MenuItem struct {
	ID         uuid.UUID
	Name       string
	PriceMinor int64
	// Available is false for items listed but not orderable right now.
	Available bool
}

// ErrNotFound marks a lookup of something that doesn't exist. The RPC layer
// turns it into NOT_FOUND.
var ErrNotFound = errors.New("not found")

// ErrInvalidArgument marks errors caused by bad input. Check with errors.Is;
// the RPC layer turns it into INVALID_ARGUMENT.
var ErrInvalidArgument = errors.New("invalid argument")
