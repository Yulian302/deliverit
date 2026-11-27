package ordering

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/Yulian302/deliverit/platform/identity"
)

// Placement is everything the store's place-order transaction needs. The
// service fills it in; the store runs the checks that need the database
// (restaurant exists, items belong to it and are available, delivery within
// MaxDeliveryKm) and returns errors wrapping ErrInvalidArgument for those.
type Placement struct {
	// Scope and Key identify the idempotency key: the customer's user ID and
	// the client's key.
	Scope, Key string
	// RequestHash fingerprints the request without its key.
	RequestHash []byte
	// NewID is the ID the order gets if this request creates it.
	NewID      uuid.UUID
	CustomerID uuid.UUID
	Input      PlaceInput
	Now        time.Time
}

// Store persists orders. Promises the service relies on:
//
//   - PlaceOrder is one transaction. If Scope/Key already has a committed
//     order, it returns that order as first returned, with replayed = true,
//     or ErrKeyReused if RequestHash differs. A concurrent request with the
//     same key waits for the first to finish. If the transaction fails, no
//     trace of the key is left, so a retry starts fresh.
//   - GetOrder returns ErrNotFound for an unknown ID.
type Store interface {
	PlaceOrder(ctx context.Context, p Placement) (o Order, replayed bool, err error)
	GetOrder(ctx context.Context, id uuid.UUID) (Order, error)
}

// Service holds the ordering rules. It knows nothing about HTTP, RPC or SQL.
type Service struct {
	store Store
	now   func() time.Time
}

// NewService returns a Service storing through s; now is the clock.
func NewService(s Store, now func() time.Time) *Service {
	return &Service{store: s, now: now}
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Place creates an order for caller, a customer, or replays the result of an
// earlier request with the same idempotency key. replayed reports which.
func (s *Service) Place(ctx context.Context, caller identity.Identity, in PlaceInput) (o Order, replayed bool, err error) {
	if caller.Role != identity.RoleCustomer {
		return Order{}, false, fmt.Errorf("%w: only customers place orders", ErrPermissionDenied)
	}
	customerID, err := uuid.Parse(caller.UserID)
	if err != nil {
		return Order{}, false, fmt.Errorf("%w: caller id is not a UUID", ErrUnauthenticated)
	}
	if err := validate(in); err != nil {
		return Order{}, false, err
	}
	hash, err := requestHash(in)
	if err != nil {
		return Order{}, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Order{}, false, err
	}
	o, replayed, err = s.store.PlaceOrder(ctx, Placement{
		Scope:       caller.UserID,
		Key:         in.IdempotencyKey,
		RequestHash: hash,
		NewID:       id,
		CustomerID:  customerID,
		Input:       in,
		Now:         s.now(),
	})
	if err != nil {
		return Order{}, false, fmt.Errorf("place order: %w", err)
	}
	return o, replayed, nil
}

// Get returns an order the caller may see: its customer, staff of its
// restaurant, or ops. For anyone else it is ErrNotFound, exactly like a
// missing order, so IDs can't be probed.
func (s *Service) Get(ctx context.Context, caller identity.Identity, id uuid.UUID) (Order, error) {
	o, err := s.store.GetOrder(ctx, id)
	if err != nil {
		return Order{}, fmt.Errorf("get order: %w", err)
	}
	if !canSee(caller, o) {
		return Order{}, fmt.Errorf("get order %s: %w", id, ErrNotFound)
	}
	return o, nil
}

func canSee(c identity.Identity, o Order) bool {
	switch c.Role {
	case identity.RoleOps:
		return true
	case identity.RoleCustomer:
		return c.UserID == o.CustomerID.String()
	case identity.RoleRestaurant:
		return c.RestaurantID == o.RestaurantID.String()
	default:
		// Couriers will see the orders they deliver once dispatch exists.
		return false
	}
}

// validate checks everything that needs no database.
func validate(in PlaceInput) error {
	if !keyPattern.MatchString(in.IdempotencyKey) {
		return invalid("idempotency key must be 1-%d characters of A-Z, a-z, 0-9, _ or -", MaxKeyLength)
	}
	if len(in.Lines) == 0 || len(in.Lines) > MaxLines {
		return invalid("an order needs 1-%d lines, got %d", MaxLines, len(in.Lines))
	}
	seen := make(map[uuid.UUID]bool, len(in.Lines))
	for i, l := range in.Lines {
		if l.Quantity < 1 || l.Quantity > MaxQuantity {
			return invalid("line %d: quantity must be 1-%d", i+1, MaxQuantity)
		}
		if seen[l.MenuItemID] {
			return invalid("line %d: menu item %s appears twice; use the quantity", i+1, l.MenuItemID)
		}
		seen[l.MenuItemID] = true
	}
	// "Inside" comparisons reject NaN and ±Inf too (see catalog.inRange).
	if !(in.Delivery.Lat >= -90 && in.Delivery.Lat <= 90) || !(in.Delivery.Lng >= -180 && in.Delivery.Lng <= 180) {
		return invalid("delivery coordinates out of range")
	}
	return nil
}

// requestHash is SHA-256 over the request without its key. JSON of these
// structs is deterministic (field order is fixed), and line order counts: a
// reordered request is a different request.
func requestHash(in PlaceInput) ([]byte, error) {
	b, err := json.Marshal(struct {
		RestaurantID uuid.UUID `json:"restaurantId"`
		Lines        []Line    `json:"lines"`
		Delivery     Delivery  `json:"delivery"`
	}{in.RestaurantID, in.Lines, in.Delivery})
	if err != nil {
		return nil, fmt.Errorf("hash request: %w", err)
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidArgument}, args...)...)
}
