package ordering

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yulian302/deliverit/services/order/internal/ordering/dbgen"
)

// PostgresStore implements Store on Postgres through sqlc-generated queries.
type PostgresStore struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore returns a store that works through pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool, q: dbgen.New(pool)}
}

// PlaceOrder implements Store in one READ COMMITTED transaction (SPEC §8.1):
//
//  1. Insert the idempotency key row. If another transaction is inserting the
//     same key right now, Postgres makes this statement wait on the primary
//     key until that one commits (then this insert does nothing) or rolls
//     back (then this insert succeeds). No "in progress" state is needed.
//  2. Key already there: replay its stored response, or ErrKeyReused if the
//     request differs.
//  3. Key is ours: check and price the lines from the database, insert the
//     order and its items, store the response on the key row, commit.
//
// Any error rolls everything back, the key row included.
func (s *PostgresStore) PlaceOrder(ctx context.Context, p Placement) (o Order, replayed bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Order{}, false, fmt.Errorf("ordering store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // always rollback. For a successful commit - it's harmless
	q := s.q.WithTx(tx)

	inserted, err := q.InsertIdempotencyKey(ctx, dbgen.InsertIdempotencyKeyParams{
		Scope: p.Scope, Key: p.Key, RequestHash: p.RequestHash,
	})
	if err != nil {
		return Order{}, false, fmt.Errorf("ordering store: insert key: %w", err)
	}
	if inserted == 0 {
		o, err = replay(ctx, q, p)
		if err != nil {
			return Order{}, false, err
		}
		return o, true, tx.Commit(ctx)
	}

	o, err = s.create(ctx, q, p)
	if err != nil {
		return Order{}, false, err
	}
	snapshot, err := json.Marshal(o)
	if err != nil {
		return Order{}, false, fmt.Errorf("ordering store: encode response: %w", err)
	}
	if err := q.SetIdempotencyResponse(ctx, dbgen.SetIdempotencyResponseParams{
		Scope: p.Scope, Key: p.Key, Response: snapshot,
	}); err != nil {
		return Order{}, false, fmt.Errorf("ordering store: store response: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, false, fmt.Errorf("ordering store: commit: %w", err)
	}
	return o, false, nil
}

// replay returns the response stored for an existing key.
func replay(ctx context.Context, q *dbgen.Queries, p Placement) (Order, error) {
	row, err := q.GetIdempotencyKey(ctx, dbgen.GetIdempotencyKeyParams{Scope: p.Scope, Key: p.Key})
	if err != nil {
		return Order{}, fmt.Errorf("ordering store: read key: %w", err)
	}
	if !bytes.Equal(row.RequestHash, p.RequestHash) {
		return Order{}, ErrKeyReused
	}
	if row.Response == nil {
		// Can't happen: the response is written before the owning transaction
		// commits, and we only see committed rows.
		return Order{}, errors.New("ordering store: idempotency key has no response")
	}
	var o Order
	if err := json.Unmarshal(row.Response, &o); err != nil {
		return Order{}, fmt.Errorf("ordering store: decode response: %w", err)
	}
	return o, nil
}

// create runs the checks that need the database, then inserts the order.
func (s *PostgresStore) create(ctx context.Context, q *dbgen.Queries, p Placement) (Order, error) {
	in := p.Input
	loc, err := q.GetRestaurantLocation(ctx, in.RestaurantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, invalid("unknown restaurant %s", in.RestaurantID)
	}
	if err != nil {
		return Order{}, fmt.Errorf("ordering store: restaurant: %w", err)
	}
	if d := DistanceKm(loc.Lat, loc.Lng, in.Delivery.Lat, in.Delivery.Lng); d > MaxDeliveryKm {
		return Order{}, invalid("delivery is %.1f km from the restaurant; the limit is %.0f km", d, MaxDeliveryKm)
	}

	ids := make([]uuid.UUID, len(in.Lines))
	for i, l := range in.Lines {
		ids[i] = l.MenuItemID
	}
	rows, err := q.GetMenuItemsByIDs(ctx, ids)
	if err != nil {
		return Order{}, fmt.Errorf("ordering store: menu items: %w", err)
	}
	menu := make(map[uuid.UUID]dbgen.MenuItem, len(rows))
	for _, m := range rows {
		menu[m.ID] = m
	}

	o := Order{
		ID:           p.NewID,
		CustomerID:   p.CustomerID,
		RestaurantID: in.RestaurantID,
		Status:       StatusPendingPayment,
		Items:        make([]Item, 0, len(in.Lines)),
		Currency:     Currency,
		Delivery:     in.Delivery,
		// Postgres keeps microseconds; truncate so the returned order equals
		// what a later read gives.
		CreatedAt: p.Now.UTC().Truncate(time.Microsecond),
		Version:   1,
	}
	for i, l := range in.Lines {
		m, ok := menu[l.MenuItemID]
		switch {
		case !ok, m.RestaurantID != in.RestaurantID:
			return Order{}, invalid("line %d: menu item %s is not on this restaurant's menu", i+1, l.MenuItemID)
		case !m.Available:
			return Order{}, invalid("line %d: %s is not available right now", i+1, m.Name)
		}
		o.Items = append(o.Items, Item{MenuItemID: m.ID, Name: m.Name, UnitPriceMinor: m.PriceMinor, Quantity: l.Quantity})
		o.TotalMinor += m.PriceMinor * int64(l.Quantity) // ≤ 20 lines × 20 × bigint price: no overflow in practice
	}

	if err := q.InsertOrder(ctx, dbgen.InsertOrderParams{
		ID:              o.ID,
		CustomerID:      o.CustomerID,
		RestaurantID:    o.RestaurantID,
		Status:          string(o.Status),
		TotalMinor:      o.TotalMinor,
		Currency:        o.Currency,
		DeliveryLat:     o.Delivery.Lat,
		DeliveryLng:     o.Delivery.Lng,
		DeliveryAddress: o.Delivery.Address,
		CreatedAt:       o.CreatedAt,
	}); err != nil {
		return Order{}, fmt.Errorf("ordering store: insert order: %w", err)
	}
	for i, it := range o.Items {
		if err := q.InsertOrderItem(ctx, dbgen.InsertOrderItemParams{
			OrderID:        o.ID,
			LineNo:         int32(i + 1),
			MenuItemID:     it.MenuItemID,
			Name:           it.Name,
			UnitPriceMinor: it.UnitPriceMinor,
			Quantity:       int32(it.Quantity), // validated: 1..MaxQuantity
		}); err != nil {
			return Order{}, fmt.Errorf("ordering store: insert item: %w", err)
		}
	}
	return o, nil
}

// GetOrder implements Store.
func (s *PostgresStore) GetOrder(ctx context.Context, id uuid.UUID) (Order, error) {
	r, err := s.q.GetOrder(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("order %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Order{}, fmt.Errorf("ordering store: get order: %w", err)
	}
	items, err := s.q.GetOrderItems(ctx, id)
	if err != nil {
		return Order{}, fmt.Errorf("ordering store: get items: %w", err)
	}
	o := Order{
		ID:           r.ID,
		CustomerID:   r.CustomerID,
		RestaurantID: r.RestaurantID,
		Status:       Status(r.Status),
		Items:        make([]Item, 0, len(items)),
		TotalMinor:   r.TotalMinor,
		Currency:     r.Currency,
		Delivery:     Delivery{Lat: r.DeliveryLat, Lng: r.DeliveryLng, Address: r.DeliveryAddress},
		CreatedAt:    r.CreatedAt.UTC(),
		Version:      int(r.Version),
	}
	for _, it := range items {
		o.Items = append(o.Items, Item{MenuItemID: it.MenuItemID, Name: it.Name, UnitPriceMinor: it.UnitPriceMinor, Quantity: int(it.Quantity)})
	}
	return o, nil
}
