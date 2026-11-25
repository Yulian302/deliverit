package catalog_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/Yulian302/deliverit/services/order/internal/catalog"
)

// fakeStore records the query it receives and returns canned results.
type fakeStore struct {
	calls int
	got   catalog.NearbyQuery
	rs    []catalog.Restaurant
	err   error
}

func (f *fakeStore) ListNear(_ context.Context, q catalog.NearbyQuery) ([]catalog.Restaurant, error) {
	f.calls++
	f.got = q
	return f.rs, f.err
}

var kyiv = catalog.Point{Lat: 50.45, Lng: 30.52}

func TestListNearby_AppliesDefaults(t *testing.T) {
	store := &fakeStore{}
	_, err := catalog.NewService(store).ListNearby(context.Background(), catalog.NearbyQuery{Center: kyiv})
	if err != nil {
		t.Fatal(err)
	}
	if store.got.RadiusKm != 5 || store.got.Limit != 20 {
		t.Errorf("store got radius=%v limit=%v, want defaults 5 and 20", store.got.RadiusKm, store.got.Limit)
	}
	if store.got.Center != kyiv {
		t.Errorf("store got center %v, want %v", store.got.Center, kyiv)
	}
}

func TestListNearby_KeepsExplicitValues(t *testing.T) {
	store := &fakeStore{}
	q := catalog.NearbyQuery{Center: kyiv, RadiusKm: 20, Limit: 50} // both at their maximum
	if _, err := catalog.NewService(store).ListNearby(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if store.got != q {
		t.Errorf("store got %+v, want %+v unchanged", store.got, q)
	}
}

func TestListNearby_RejectsInvalid(t *testing.T) {
	nan := math.NaN()
	for name, q := range map[string]catalog.NearbyQuery{
		"lat 91":    {Center: catalog.Point{Lat: 91, Lng: 0}},
		"lat -91":   {Center: catalog.Point{Lat: -91, Lng: 0}},
		"lng 181":   {Center: catalog.Point{Lat: 0, Lng: 181}},
		"lng -181":  {Center: catalog.Point{Lat: 0, Lng: -181}},
		"radius -1": {Center: kyiv, RadiusKm: -1},
		"radius 21": {Center: kyiv, RadiusKm: 21},
		"limit -1":  {Center: kyiv, Limit: -1},
		"limit 51":  {Center: kyiv, Limit: 51},
		// NaN fails every comparison, so a naive range check lets it through.
		"lat NaN":    {Center: catalog.Point{Lat: nan, Lng: 0}},
		"lng NaN":    {Center: catalog.Point{Lat: 0, Lng: nan}},
		"radius NaN": {Center: kyiv, RadiusKm: nan},
		"lat +Inf":   {Center: catalog.Point{Lat: math.Inf(1), Lng: 0}},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			_, err := catalog.NewService(store).ListNearby(context.Background(), q)
			if !errors.Is(err, catalog.ErrInvalidArgument) {
				t.Errorf("err = %v, want ErrInvalidArgument", err)
			}
			if store.calls != 0 {
				t.Error("store was called with invalid input")
			}
		})
	}
}

func TestListNearby_PropagatesStoreError(t *testing.T) {
	boom := errors.New("db down")
	_, err := catalog.NewService(&fakeStore{err: boom}).ListNearby(context.Background(), catalog.NearbyQuery{Center: kyiv})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the store error", err)
	}
	if errors.Is(err, catalog.ErrInvalidArgument) {
		t.Error("a store failure must not look like bad input")
	}
}

func TestListNearby_ReturnsStoreResults(t *testing.T) {
	want := []catalog.Restaurant{{Name: "A"}, {Name: "B"}}
	got, err := catalog.NewService(&fakeStore{rs: want}).ListNearby(context.Background(), catalog.NearbyQuery{Center: kyiv})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "A" || got[1].Name != "B" {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
