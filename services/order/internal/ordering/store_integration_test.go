//go:build integration

package ordering_test

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Yulian302/deliverit/platform/pg"
	"github.com/Yulian302/deliverit/services/order/db/migrations"
	"github.com/Yulian302/deliverit/services/order/internal/ordering"
)

// pool is a migrated and seeded database. Tests use fresh customer IDs, so
// they don't see each other's orders.
var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("order"), postgres.WithUsername("order"), postgres.WithPassword("order"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		log.Fatalf("start postgres: %v", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	if err := pg.Migrate(ctx, dsn, migrations.FS); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if pool, err = pg.Connect(ctx, dsn); err != nil {
		log.Fatal(err)
	}
	code := m.Run()
	pool.Close()
	_ = testcontainers.TerminateContainer(ctr)
	os.Exit(code)
}

var unavailable = uuid.MustParse("0193a1b2-0000-7000-8000-000000000103") // Khreshchatyk item 3
var podilItem = uuid.MustParse("0193a1b2-0000-7000-8000-000000000501")

// placement builds a valid placement for a fresh customer; hash is fixed per
// test so duplicates match.
func placement(t *testing.T, customer uuid.UUID, key string, in ordering.PlaceInput) ordering.Placement {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	in.IdempotencyKey = key
	return ordering.Placement{
		Scope: customer.String(), Key: key, RequestHash: []byte("hash-" + key),
		NewID: id, CustomerID: customer, Input: in, Now: t0,
	}
}

func countOrders(t *testing.T, customer uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM orders WHERE customer_id = $1", customer).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func menuPrice(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	var p int64
	if err := pool.QueryRow(context.Background(), "SELECT price_minor FROM menu_items WHERE id = $1", id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlaceOrder_PricesFromDatabase(t *testing.T) {
	store := ordering.NewPostgresStore(pool)
	o, replayed, err := store.PlaceOrder(context.Background(), placement(t, uuid.New(), "k1", validInput()))
	if err != nil || replayed {
		t.Fatalf("PlaceOrder = replayed %v, %v", replayed, err)
	}
	want := 2*menuPrice(t, chickenKyi) + menuPrice(t, borscht)
	if o.TotalMinor != want || o.Currency != "UAH" || o.Status != ordering.StatusPendingPayment || o.Version != 1 {
		t.Errorf("order = total %d (want %d) %s %s v%d", o.TotalMinor, want, o.Currency, o.Status, o.Version)
	}
	if len(o.Items) != 2 || o.Items[0].Name != "Chicken Kyiv" || o.Items[0].UnitPriceMinor != menuPrice(t, chickenKyi) || o.Items[0].Quantity != 2 {
		t.Errorf("items = %+v", o.Items)
	}
}

// Review Focus 1: SPEC §14 — 10 concurrent duplicates, one order.
func TestPlaceOrder_ConcurrentDuplicatesCreateOneOrder(t *testing.T) {
	store := ordering.NewPostgresStore(pool)
	customer := uuid.New()
	p := placement(t, customer, "dup", validInput())

	const n = 10
	var (
		wg       sync.WaitGroup
		orders   [n]ordering.Order
		replayed [n]bool
		errs     [n]error
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := p
			q.NewID = uuid.Must(uuid.NewV7()) // each attempt would create its own ID
			orders[i], replayed[i], errs[i] = store.PlaceOrder(context.Background(), q)
		}()
	}
	wg.Wait()

	firsts := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if orders[i].ID != orders[0].ID || orders[i].TotalMinor != orders[0].TotalMinor || !orders[i].CreatedAt.Equal(orders[0].CreatedAt) {
			t.Errorf("attempt %d got order %s; want %s", i, orders[i].ID, orders[0].ID)
		}
		if !replayed[i] {
			firsts++
		}
	}
	if firsts != 1 {
		t.Errorf("%d attempts created an order; want exactly 1", firsts)
	}
	if got := countOrders(t, customer); got != 1 {
		t.Fatalf("%d orders in the database; want 1", got)
	}
}

// Review Focus 3.
func TestPlaceOrder_KeyReuseWithDifferentRequest(t *testing.T) {
	store := ordering.NewPostgresStore(pool)
	customer := uuid.New()
	if _, _, err := store.PlaceOrder(context.Background(), placement(t, customer, "reuse", validInput())); err != nil {
		t.Fatal(err)
	}
	p := placement(t, customer, "reuse", validInput())
	p.RequestHash = []byte("something else")
	if _, _, err := store.PlaceOrder(context.Background(), p); !errors.Is(err, ordering.ErrKeyReused) {
		t.Fatalf("err = %v; want ErrKeyReused", err)
	}
	if got := countOrders(t, customer); got != 1 {
		t.Fatalf("%d orders; want 1", got)
	}
}

// Review Focus 2.
func TestPlaceOrder_FailedAttemptDoesNotKeepKey(t *testing.T) {
	store := ordering.NewPostgresStore(pool)
	customer := uuid.New()
	bad := validInput()
	bad.Lines = append(bad.Lines, ordering.Line{MenuItemID: unavailable, Quantity: 1})
	if _, _, err := store.PlaceOrder(context.Background(), placement(t, customer, "retry", bad)); !errors.Is(err, ordering.ErrInvalidArgument) {
		t.Fatalf("first attempt err = %v; want ErrInvalidArgument", err)
	}
	var keys int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM idempotency_keys WHERE scope = $1", customer.String()).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 0 {
		t.Fatalf("%d key rows left after a failed attempt; want 0", keys)
	}
	p := placement(t, customer, "retry", validInput())
	p.RequestHash = []byte("fixed request")
	o, replayed, err := store.PlaceOrder(context.Background(), p)
	if err != nil || replayed || o.ID != p.NewID {
		t.Fatalf("retry = %s replayed %v, %v; want a new order", o.ID, replayed, err)
	}
}

// Review Focus 4.
func TestPlaceOrder_KeysAreScopedPerCustomer(t *testing.T) {
	store := ordering.NewPostgresStore(pool)
	a, _, errA := store.PlaceOrder(context.Background(), placement(t, uuid.New(), "same-key", validInput()))
	b, _, errB := store.PlaceOrder(context.Background(), placement(t, uuid.New(), "same-key", validInput()))
	if errA != nil || errB != nil || a.ID == b.ID {
		t.Fatalf("orders %s, %s (%v, %v); want two different orders", a.ID, b.ID, errA, errB)
	}
}

func TestPlaceOrder_RejectsForeignAndFarAway(t *testing.T) {
	store := ordering.NewPostgresStore(pool)
	foreign := validInput()
	foreign.Lines = []ordering.Line{{MenuItemID: podilItem, Quantity: 1}}
	far := validInput()
	far.Delivery.Lat += 0.1 // ~11 km north of Khreshchatyk Kitchen
	unknownRestaurant := validInput()
	unknownRestaurant.RestaurantID = uuid.New()
	unknownItem := validInput()
	unknownItem.Lines = []ordering.Line{{MenuItemID: uuid.New(), Quantity: 1}}

	for name, in := range map[string]ordering.PlaceInput{
		"item of another restaurant": foreign,
		"delivery 11 km away":        far,
		"unknown restaurant":         unknownRestaurant,
		"unknown item":               unknownItem,
	} {
		t.Run(name, func(t *testing.T) {
			customer := uuid.New()
			if _, _, err := store.PlaceOrder(context.Background(), placement(t, customer, "x", in)); !errors.Is(err, ordering.ErrInvalidArgument) {
				t.Fatalf("err = %v; want ErrInvalidArgument", err)
			}
			if got := countOrders(t, customer); got != 0 {
				t.Fatalf("%d orders; want 0", got)
			}
		})
	}
}

func TestGetOrder_RoundTrip(t *testing.T) {
	store := ordering.NewPostgresStore(pool)
	placed, _, err := store.PlaceOrder(context.Background(), placement(t, uuid.New(), "get", validInput()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.GetOrder(context.Background(), placed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != placed.ID || got.TotalMinor != placed.TotalMinor || len(got.Items) != len(placed.Items) ||
		got.Items[1] != placed.Items[1] || got.Delivery != placed.Delivery || !got.CreatedAt.Equal(placed.CreatedAt) {
		t.Errorf("GetOrder = %+v\nwant      %+v", got, placed)
	}
	if _, err := store.GetOrder(context.Background(), uuid.New()); !errors.Is(err, ordering.ErrNotFound) {
		t.Fatalf("unknown id: err = %v; want ErrNotFound", err)
	}
}
