package ordering_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Yulian302/deliverit/platform/identity"
	"github.com/Yulian302/deliverit/services/order/internal/ordering"
)

var (
	t0         = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	olenaID    = uuid.MustParse("0193a1b1-0000-7000-8000-0000000000c1")
	olena      = identity.Identity{UserID: olenaID.String(), Role: identity.RoleCustomer}
	khreshchat = uuid.MustParse("0193a1b0-0000-7000-8000-000000000001")
	chickenKyi = uuid.MustParse("0193a1b2-0000-7000-8000-000000000101")
	borscht    = uuid.MustParse("0193a1b2-0000-7000-8000-000000000102")
)

// fakeStore records what it's asked and returns canned answers.
type fakeStore struct {
	calls     int
	placement ordering.Placement
	order     ordering.Order
	replayed  bool
	err       error
}

func (f *fakeStore) PlaceOrder(_ context.Context, p ordering.Placement) (ordering.Order, bool, error) {
	f.calls++
	f.placement = p
	return f.order, f.replayed, f.err
}

func (f *fakeStore) GetOrder(_ context.Context, _ uuid.UUID) (ordering.Order, error) {
	f.calls++
	return f.order, f.err
}

func validInput() ordering.PlaceInput {
	return ordering.PlaceInput{
		IdempotencyKey: "order-1",
		RestaurantID:   khreshchat,
		Lines:          []ordering.Line{{MenuItemID: chickenKyi, Quantity: 2}, {MenuItemID: borscht, Quantity: 1}},
		Delivery:       ordering.Delivery{Lat: 50.45, Lng: 30.52, Address: "Khreshchatyk 1, flat 5"},
	}
}

func newService(s ordering.Store) *ordering.Service {
	return ordering.NewService(s, func() time.Time { return t0 })
}

func TestPlace_RequiresCustomer(t *testing.T) {
	store := &fakeStore{}
	courier := identity.Identity{UserID: uuid.NewString(), Role: identity.RoleCourier}
	_, _, err := newService(store).Place(context.Background(), courier, validInput())
	if !errors.Is(err, ordering.ErrPermissionDenied) || store.calls != 0 {
		t.Fatalf("err = %v, store calls = %d; want ErrPermissionDenied, 0", err, store.calls)
	}
}

func TestPlace_Validation(t *testing.T) {
	lines := func(n int) []ordering.Line {
		out := make([]ordering.Line, n)
		for i := range out {
			out[i] = ordering.Line{MenuItemID: uuid.New(), Quantity: 1}
		}
		return out
	}
	cases := map[string]func(*ordering.PlaceInput){
		"empty key":      func(in *ordering.PlaceInput) { in.IdempotencyKey = "" },
		"65-char key":    func(in *ordering.PlaceInput) { in.IdempotencyKey = strings.Repeat("k", 65) },
		"key with space": func(in *ordering.PlaceInput) { in.IdempotencyKey = "a b" },
		"no lines":       func(in *ordering.PlaceInput) { in.Lines = nil },
		"21 lines":       func(in *ordering.PlaceInput) { in.Lines = lines(21) },
		"quantity 0":     func(in *ordering.PlaceInput) { in.Lines[0].Quantity = 0 },
		"quantity 21":    func(in *ordering.PlaceInput) { in.Lines[0].Quantity = 21 },
		"item twice":     func(in *ordering.PlaceInput) { in.Lines[1].MenuItemID = in.Lines[0].MenuItemID },
		"lat 91":         func(in *ordering.PlaceInput) { in.Delivery.Lat = 91 },
		"lng NaN":        func(in *ordering.PlaceInput) { in.Delivery.Lng = math.NaN() },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			in := validInput()
			mutate(&in)
			_, _, err := newService(store).Place(context.Background(), olena, in)
			if !errors.Is(err, ordering.ErrInvalidArgument) || store.calls != 0 {
				t.Fatalf("err = %v, store calls = %d; want ErrInvalidArgument, 0", err, store.calls)
			}
		})
	}
}

func placementFor(t *testing.T, in ordering.PlaceInput) ordering.Placement {
	t.Helper()
	store := &fakeStore{}
	if _, _, err := newService(store).Place(context.Background(), olena, in); err != nil {
		t.Fatal(err)
	}
	return store.placement
}

func TestPlace_HashIgnoresKeyButNotBody(t *testing.T) {
	a := validInput()
	b := validInput()
	b.IdempotencyKey = "order-2"
	c := validInput()
	c.Lines[0].Quantity = 3

	ha, hb, hc := placementFor(t, a).RequestHash, placementFor(t, b).RequestHash, placementFor(t, c).RequestHash
	if len(ha) == 0 || string(ha) != string(hb) {
		t.Error("hash depends on the key")
	}
	if string(ha) == string(hc) {
		t.Error("hash ignores the quantity")
	}
}

func TestPlace_PassesCallerAndClock(t *testing.T) {
	p := placementFor(t, validInput())
	if p.Scope != olena.UserID || p.Key != "order-1" || p.CustomerID != olenaID || !p.Now.Equal(t0) {
		t.Errorf("placement = scope %q key %q customer %s now %v", p.Scope, p.Key, p.CustomerID, p.Now)
	}
	if p.NewID.Version() != 7 {
		t.Errorf("NewID version = %d; want 7", p.NewID.Version())
	}
}

// Review Focus 5: who may read an order.
func TestGet_Visibility(t *testing.T) {
	order := ordering.Order{ID: uuid.New(), CustomerID: olenaID, RestaurantID: khreshchat, Status: ordering.StatusPendingPayment}
	cases := map[string]struct {
		caller identity.Identity
		ok     bool
	}{
		"its customer":      {olena, true},
		"another customer":  {identity.Identity{UserID: uuid.NewString(), Role: identity.RoleCustomer}, false},
		"its restaurant":    {identity.Identity{UserID: uuid.NewString(), Role: identity.RoleRestaurant, RestaurantID: khreshchat.String()}, true},
		"other restaurant":  {identity.Identity{UserID: uuid.NewString(), Role: identity.RoleRestaurant, RestaurantID: uuid.NewString()}, false},
		"ops":               {identity.Identity{UserID: uuid.NewString(), Role: identity.RoleOps}, true},
		"courier (for now)": {identity.Identity{UserID: uuid.NewString(), Role: identity.RoleCourier}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := newService(&fakeStore{order: order}).Get(context.Background(), tc.caller, order.ID)
			switch {
			case tc.ok && (err != nil || got.ID != order.ID):
				t.Fatalf("Get = %v, %v; want the order", got.ID, err)
			case !tc.ok && !errors.Is(err, ordering.ErrNotFound):
				t.Fatalf("err = %v; want ErrNotFound", err)
			}
		})
	}
}
