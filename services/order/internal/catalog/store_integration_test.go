//go:build integration

package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Yulian302/deliverit/platform/pg"
	"github.com/Yulian302/deliverit/services/order/db/migrations"
	"github.com/Yulian302/deliverit/services/order/internal/catalog"
)

// pool is a migrated and seeded database shared by the store tests, which
// only read from it.
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
	if err := pg.Migrate(ctx, dsn, migrations.FS); err != nil { // the real migrations, seed included
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

var kyivCentre = catalog.Point{Lat: 50.4501, Lng: 30.5234}

func listNear(t *testing.T, q catalog.NearbyQuery) []catalog.Restaurant {
	t.Helper()
	rs, err := catalog.NewPostgresStore(pool).ListNear(context.Background(), q)
	if err != nil {
		t.Fatalf("ListNear(%+v): %v", q, err)
	}
	return rs
}

// haversineKm is an independent distance formula, so the tests don't just
// repeat the SQL's own math back to it.
func haversineKm(a, b catalog.Point) float64 {
	const r = 6371.0
	rad := math.Pi / 180
	dLat, dLng := (b.Lat-a.Lat)*rad, (b.Lng-a.Lng)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(a.Lat*rad)*math.Cos(b.Lat*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * r * math.Asin(math.Sqrt(h))
}

func TestListNear_SeedOrderedByDistance(t *testing.T) {
	rs := listNear(t, catalog.NearbyQuery{Center: kyivCentre, RadiusKm: 5, Limit: 50})

	if len(rs) != 10 {
		t.Fatalf("got %d restaurants within 5 km, want all 10 seeded", len(rs))
	}
	if !sort.SliceIsSorted(rs, func(i, j int) bool { return rs[i].DistanceKm < rs[j].DistanceKm }) {
		t.Error("results are not ordered nearest first")
	}
	for _, r := range rs {
		if want := haversineKm(kyivCentre, r.Location); math.Abs(r.DistanceKm-want) > 0.001 {
			t.Errorf("%s: DistanceKm = %.4f, independent haversine = %.4f", r.Name, r.DistanceKm, want)
		}
		if r.ID.String() == "" || r.Name == "" || r.PrepMinutes <= 0 {
			t.Errorf("incompletely mapped row: %+v", r)
		}
	}
}

// The radius filter (with the bounding box in front of it) must return
// exactly the restaurants a full scan would: nothing dropped, nothing extra.
func TestListNear_RadiusMatchesFullScan(t *testing.T) {
	all := listNear(t, catalog.NearbyQuery{Center: kyivCentre, RadiusKm: 20, Limit: 50})
	if len(all) != 10 {
		t.Fatalf("baseline found %d restaurants, want all 10 seeded", len(all)) // else the comparison below proves nothing
	}

	for _, radius := range []float64{0.1, 0.5, 1, 1.5, 2, 3, 4} {
		want := map[string]bool{}
		for _, r := range all {
			if haversineKm(kyivCentre, r.Location) <= radius {
				want[r.Name] = true
			}
		}
		got := listNear(t, catalog.NearbyQuery{Center: kyivCentre, RadiusKm: radius, Limit: 50})
		if len(got) != len(want) {
			t.Errorf("radius %v km: got %d restaurants, full scan finds %d", radius, len(got), len(want))
		}
		for _, r := range got {
			if !want[r.Name] {
				t.Errorf("radius %v km: %s (%.3f km) should not be included", radius, r.Name, r.DistanceKm)
			}
		}
	}
}

func TestListNear_Limit(t *testing.T) {
	rs := listNear(t, catalog.NearbyQuery{Center: kyivCentre, RadiusKm: 5, Limit: 3})
	if len(rs) != 3 {
		t.Fatalf("got %d, want 3", len(rs))
	}
	all := listNear(t, catalog.NearbyQuery{Center: kyivCentre, RadiusKm: 5, Limit: 50})
	for i := range rs {
		if rs[i].ID != all[i].ID {
			t.Errorf("limit 3 returned %s at %d, want the 3 nearest", rs[i].Name, i)
		}
	}
}

func TestListNear_NothingNearbyIsEmptyNotNil(t *testing.T) {
	rs := listNear(t, catalog.NearbyQuery{Center: catalog.Point{Lat: 0, Lng: 0}, RadiusKm: 5, Limit: 20})
	if rs == nil || len(rs) != 0 {
		t.Errorf("got %#v, want an empty, non-nil slice", rs)
	}
}

func TestStoreMenu_Seeded(t *testing.T) {
	id := uuid.MustParse("0193a1b0-0000-7000-8000-000000000001")
	r, items, err := catalog.NewPostgresStore(pool).Menu(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id || r.Name != "Khreshchatyk Kitchen" {
		t.Errorf("restaurant = %+v", r)
	}
	if len(items) != 3 {
		t.Fatalf("%d items; want 3", len(items))
	}
	for i, it := range items {
		if want := uuid.MustParse(fmt.Sprintf("0193a1b2-0000-7000-8000-0000000001%02d", i+1)); it.ID != want {
			t.Errorf("item %d id = %s; want %s (ordered by id)", i, it.ID, want)
		}
		if it.PriceMinor <= 0 || it.Name == "" {
			t.Errorf("item %d = %+v", i, it)
		}
	}
	if items[2].Available || !items[0].Available {
		t.Errorf("availability = %v,%v,%v; want true,true,false", items[0].Available, items[1].Available, items[2].Available)
	}
}

func TestStoreMenu_UnknownRestaurant(t *testing.T) {
	_, _, err := catalog.NewPostgresStore(pool).Menu(context.Background(), uuid.New())
	if !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
}
