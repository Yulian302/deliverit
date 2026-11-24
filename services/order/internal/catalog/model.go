// Package catalog is Order's restaurant catalog: finding restaurants near a
// point.
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

// ErrInvalidArgument marks errors caused by bad input. Check with errors.Is;
// the RPC layer turns it into INVALID_ARGUMENT.
var ErrInvalidArgument = errors.New("invalid argument")
