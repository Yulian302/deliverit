package catalog_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
	"github.com/Yulian302/deliverit/services/order/internal/catalog"
)

// rpcClient serves the real handler (real Service, fake store) and returns a
// real generated client for it, plus the handler's log output.
func rpcClient(t *testing.T, store catalog.Store) (orderv1connect.OrderServiceClient, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))

	mux := http.NewServeMux()
	mux.Handle(orderv1connect.NewOrderServiceHandler(catalog.NewHandler(catalog.NewService(store), log)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return orderv1connect.NewOrderServiceClient(srv.Client(), srv.URL), &logs
}

func TestRPC_ListRestaurants_MapsFields(t *testing.T) {
	id := uuid.MustParse("0193a1b0-0000-7000-8000-000000000001")
	store := &fakeStore{rs: []catalog.Restaurant{{
		ID: id, Name: "Khreshchatyk Kitchen", Location: catalog.Point{Lat: 50.448, Lng: 30.522},
		PrepMinutes: 15, DistanceKm: 0.25,
	}}}
	client, _ := rpcClient(t, store)

	resp, err := client.ListRestaurants(context.Background(), &orderv1.ListRestaurantsRequest{
		Lat: 50.45, Lng: 30.52, RadiusKm: 3, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Request → query
	want := catalog.NearbyQuery{Center: catalog.Point{Lat: 50.45, Lng: 30.52}, RadiusKm: 3, Limit: 5}
	if store.got != want {
		t.Errorf("store got %+v, want %+v", store.got, want)
	}
	// Restaurant → response
	if n := len(resp.GetRestaurants()); n != 1 {
		t.Fatalf("got %d restaurants, want 1", n)
	}
	r := resp.GetRestaurants()[0]
	if r.GetId() != id.String() || r.GetName() != "Khreshchatyk Kitchen" || r.GetLat() != 50.448 ||
		r.GetLng() != 30.522 || r.GetPrepMinutes() != 15 || r.GetDistanceKm() != 0.25 {
		t.Errorf("response restaurant = %v", r)
	}
}

func TestRPC_InvalidArgument(t *testing.T) {
	client, _ := rpcClient(t, &fakeStore{})

	_, err := client.ListRestaurants(context.Background(), &orderv1.ListRestaurantsRequest{Lat: 100})

	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("code = %v (err %v), want invalid_argument", got, err)
	}
}

func TestRPC_StoreErrorIsInternalWithoutDetails(t *testing.T) {
	client, logs := rpcClient(t, &fakeStore{err: errors.New("pq: secret detail")})

	_, err := client.ListRestaurants(context.Background(), &orderv1.ListRestaurantsRequest{Lat: 50, Lng: 30})

	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Errorf("code = %v, want internal", got)
	}
	if strings.Contains(err.Error(), "secret detail") {
		t.Errorf("internal error leaked to the caller: %v", err)
	}
	if !strings.Contains(logs.String(), "secret detail") {
		t.Errorf("internal error not logged on the server: %q", logs.String())
	}
}

// A caller's deadline that runs out inside the store is the caller's timeout,
// not a server bug: it must reach the Gateway as DEADLINE_EXCEEDED (its 504),
// not INTERNAL, and must not be logged as a server error.
func TestRPC_DeadlineIsDeadlineExceeded(t *testing.T) {
	client, logs := rpcClient(t, &fakeStore{err: fmt.Errorf("query: %w", context.DeadlineExceeded)})

	_, err := client.ListRestaurants(context.Background(), &orderv1.ListRestaurantsRequest{Lat: 50, Lng: 30})

	if got := connect.CodeOf(err); got != connect.CodeDeadlineExceeded {
		t.Errorf("code = %v, want deadline_exceeded", got)
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("a caller timeout was logged as a server error: %q", logs.String())
	}
}

func TestRPC_EmptyResult(t *testing.T) {
	client, _ := rpcClient(t, &fakeStore{rs: []catalog.Restaurant{}})

	resp, err := client.ListRestaurants(context.Background(), &orderv1.ListRestaurantsRequest{Lat: 0, Lng: 0})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.GetRestaurants()); n != 0 {
		t.Errorf("got %d restaurants, want 0", n)
	}
}

func TestRPC_GetMenu_BadIDIsInvalidArgument(t *testing.T) {
	client, _ := rpcClient(t, &fakeStore{})
	_, err := client.GetMenu(context.Background(), &orderv1.GetMenuRequest{RestaurantId: "nope"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err = %v; want invalid_argument", err)
	}
}

func TestRPC_GetMenu_NotFound(t *testing.T) {
	client, _ := rpcClient(t, &fakeStore{menuErr: catalog.ErrNotFound})
	_, err := client.GetMenu(context.Background(), &orderv1.GetMenuRequest{RestaurantId: uuid.NewString()})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("err = %v; want not_found", err)
	}
}

func TestRPC_GetMenu_MapsFields(t *testing.T) {
	r := catalog.Restaurant{ID: uuid.New(), Name: "Fake Pho", Location: catalog.Point{Lat: 50.44, Lng: 30.54}, PrepMinutes: 12}
	item := catalog.MenuItem{ID: uuid.New(), Name: "Pho bo", PriceMinor: 21000, Available: false}
	client, _ := rpcClient(t, &fakeStore{menuRestaurant: r, menuItems: []catalog.MenuItem{item}})
	resp, err := client.GetMenu(context.Background(), &orderv1.GetMenuRequest{RestaurantId: r.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCurrency() != "UAH" || resp.GetRestaurant().GetName() != "Fake Pho" || resp.GetRestaurant().GetPrepMinutes() != 12 {
		t.Errorf("restaurant/currency = %v / %q", resp.GetRestaurant(), resp.GetCurrency())
	}
	got := resp.GetItems()
	if len(got) != 1 || got[0].GetId() != item.ID.String() || got[0].GetPriceMinor() != 21000 || got[0].GetAvailable() {
		t.Errorf("items = %v", got)
	}
}
