// Package ordering is Order's order lifecycle: placing an order (with
// idempotency keys), and reading it back. Payment, cancellation and the
// outbox come in later tasks.
//
// Layers, one file each (enforced by depguard in .golangci.yml):
//
//	rpc.go      transport: protobuf ⇄ model, errors → Connect codes
//	service.go  business rules: validation, request hashing, visibility
//	store.go    storage: the place-order transaction, through sqlc in dbgen/
//	model.go    the types all layers share
package ordering

import (
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
)

// Status is where an order is in its state machine (SPEC §5).
type Status string

// The order states, exactly as SPEC §5 names them.
const (
	StatusPendingPayment     Status = "PENDING_PAYMENT"
	StatusAwaitingRestaurant Status = "AWAITING_RESTAURANT"
	StatusPreparing          Status = "PREPARING"
	StatusReady              Status = "READY"
	StatusPickedUp           Status = "PICKED_UP"
	StatusDelivered          Status = "DELIVERED"
	StatusCancelled          Status = "CANCELLED"
	StatusFailed             Status = "FAILED"
)

// Currency is the one currency the service works in (ISO 4217); amounts are
// integer minor units of it (kopecks).
const Currency = "UAH"

// Limits on an order (business rules, not SPEC values).
const (
	MaxLines      = 20
	MaxQuantity   = 20
	MaxDeliveryKm = 10.0
	MaxKeyLength  = 64
)

// Errors the RPC layer maps to Connect codes. Check with errors.Is.
var (
	ErrInvalidArgument  = errors.New("invalid argument")
	ErrNotFound         = errors.New("not found")
	ErrKeyReused        = errors.New("idempotency key reused with a different request")
	ErrUnauthenticated  = errors.New("unauthenticated")
	ErrPermissionDenied = errors.New("permission denied")
)

// Line is one requested item: which and how many.
type Line struct {
	MenuItemID uuid.UUID `json:"menuItemId"`
	Quantity   int       `json:"quantity"`
}

// Delivery is where the order goes.
type Delivery struct {
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	Address string  `json:"address"`
}

// PlaceInput is a customer's order request.
type PlaceInput struct {
	IdempotencyKey string
	RestaurantID   uuid.UUID
	Lines          []Line
	Delivery       Delivery
}

// Item is an order line as ordered: name and price are copied from the menu
// at order time.
type Item struct {
	MenuItemID     uuid.UUID `json:"menuItemId"`
	Name           string    `json:"name"`
	UnitPriceMinor int64     `json:"unitPriceMinor"`
	Quantity       int       `json:"quantity"`
}

// Order is an order as stored. The JSON tags define the snapshot kept on the
// idempotency key row, which is what a replayed request gets back.
type Order struct {
	ID           uuid.UUID `json:"id"`
	CustomerID   uuid.UUID `json:"customerId"`
	RestaurantID uuid.UUID `json:"restaurantId"`
	Status       Status    `json:"status"`
	Items        []Item    `json:"items"`
	TotalMinor   int64     `json:"totalMinor"`
	Currency     string    `json:"currency"`
	Delivery     Delivery  `json:"delivery"`
	CreatedAt    time.Time `json:"createdAt"`
	Version      int       `json:"version"`
}

// DistanceKm is the great-circle (haversine) distance between two points
// given in degrees.
func DistanceKm(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusKm = 6371.0
	rad := math.Pi / 180
	dLat, dLng := (lat2-lat1)*rad, (lng2-lng1)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusKm * math.Asin(math.Sqrt(h))
}
