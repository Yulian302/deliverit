package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/services/gateway/internal/api"
)

// The rest of OrderServiceClient for fakeOrder (restaurants_test.go): each
// records its request and answers with the matching canned response or err.

func (f *fakeOrder) GetMenu(_ context.Context, req *orderv1.GetMenuRequest) (*orderv1.GetMenuResponse, error) {
	f.gotMenu = req
	return f.menu, f.err
}

func (f *fakeOrder) PlaceOrder(_ context.Context, req *orderv1.PlaceOrderRequest) (*orderv1.PlaceOrderResponse, error) {
	f.gotPlace = req
	return f.placed, f.err
}

func (f *fakeOrder) GetOrder(_ context.Context, req *orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error) {
	f.gotGet = req
	return f.order, f.err
}

const (
	khreshchatykID = "0193a1b0-0000-7000-8000-000000000001"
	chickenKyivID  = "0193a1b2-0000-7000-8000-000000000101"
	orderID        = "0193a1c0-0000-7000-8000-000000000001"
)

var created = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func sampleOrder() *orderv1.Order {
	return &orderv1.Order{
		Id: orderID, CustomerId: olena, RestaurantId: khreshchatykID,
		Status:     orderv1.OrderStatus_ORDER_STATUS_PENDING_PAYMENT,
		Items:      []*orderv1.OrderItem{{MenuItemId: chickenKyivID, Name: "Chicken Kyiv", UnitPriceMinor: 26500, Quantity: 2}},
		TotalMinor: 53000, Currency: "UAH",
		Delivery:  &orderv1.DeliveryLocation{Lat: 50.45, Lng: 30.52, Address: "Khreshchatyk 1"},
		CreatedAt: timestamppb.New(created), Version: 1,
	}
}

// --- menu ---

func serveMenu(f *fakeOrder, id string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/restaurants/{id}/menu", api.NewMenuHandler(f, discard))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/restaurants/"+id+"/menu", nil))
	return rec
}

func TestMenu_OK(t *testing.T) {
	f := &fakeOrder{menu: &orderv1.GetMenuResponse{
		Restaurant: &orderv1.Restaurant{Id: khreshchatykID, Name: "Khreshchatyk Kitchen"},
		Currency:   "UAH",
		Items:      []*orderv1.MenuItem{{Id: chickenKyivID, Name: "Chicken Kyiv", PriceMinor: 26500, Available: true}},
	}}
	rec := serveMenu(f, khreshchatykID)
	if rec.Code != http.StatusOK || f.gotMenu.GetRestaurantId() != khreshchatykID {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		Restaurant struct{ ID, Name string }
		Currency   string
		Items      []struct {
			ID, Name   string
			PriceMinor int64
			Available  bool
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Restaurant.Name != "Khreshchatyk Kitchen" || out.Currency != "UAH" || len(out.Items) != 1 ||
		out.Items[0].PriceMinor != 26500 || !out.Items[0].Available || out.Items[0].ID != chickenKyivID {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestMenu_Errors(t *testing.T) {
	if rec := serveMenu(&fakeOrder{}, "x"); rec.Code != http.StatusBadRequest || decodeProblemCode(t, rec) != api.CodeInvalidArgument {
		t.Errorf("bad id: status %d", rec.Code)
	}
	nf := &fakeOrder{err: connect.NewError(connect.CodeNotFound, nil)}
	if rec := serveMenu(nf, khreshchatykID); rec.Code != http.StatusNotFound || decodeProblemCode(t, rec) != api.CodeNotFound {
		t.Errorf("not found: status %d", rec.Code)
	}
}

// --- place ---

const placeBody = `{"restaurantId":"` + khreshchatykID + `","items":[{"menuItemId":"` + chickenKyivID + `","quantity":2}],` +
	`"delivery":{"lat":50.45,"lng":30.52,"address":"Khreshchatyk 1"}}`

func place(f *fakeOrder, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	api.NewPlaceOrderHandler(f, discard).ServeHTTP(rec, req)
	return rec
}

func TestPlaceOrder_Accepted(t *testing.T) {
	f := &fakeOrder{placed: &orderv1.PlaceOrderResponse{Order: sampleOrder()}}
	rec := place(f, "order-1", placeBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Location"); got != "/v1/orders/"+orderID {
		t.Errorf("Location = %q", got)
	}
	if rec.Header().Get("Idempotent-Replayed") != "" {
		t.Error("first response marked as replayed")
	}
	g := f.gotPlace
	if g.GetIdempotencyKey() != "order-1" || g.GetRestaurantId() != khreshchatykID || len(g.GetLines()) != 1 ||
		g.GetLines()[0].GetQuantity() != 2 || g.GetDelivery().GetAddress() != "Khreshchatyk 1" {
		t.Errorf("Order got %v", g)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["id"] != orderID || out["status"] != "PENDING_PAYMENT" || out["totalMinor"] != float64(53000) ||
		out["currency"] != "UAH" || out["createdAt"] != "2026-11-27T12:00:00Z" || out["version"] != float64(1) {
		t.Errorf("body = %s", rec.Body)
	}
	item := out["items"].([]any)[0].(map[string]any)
	if item["name"] != "Chicken Kyiv" || item["unitPriceMinor"] != float64(26500) || item["quantity"] != float64(2) {
		t.Errorf("item = %v", item)
	}
}

func TestPlaceOrder_ReplayIsMarked(t *testing.T) {
	f := &fakeOrder{placed: &orderv1.PlaceOrderResponse{Order: sampleOrder(), Replayed: true}}
	rec := place(f, "order-1", placeBody)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("status %d, Idempotent-Replayed %q", rec.Code, rec.Header().Get("Idempotent-Replayed"))
	}
}

func TestPlaceOrder_BadRequests(t *testing.T) {
	cases := map[string]struct{ key, body string }{
		"no key":         {"", placeBody},
		"key with space": {"a b", placeBody},
		"unknown field":  {"k", `{"restaurantId":"x","extra":1}`},
		"not json":       {"k", `nope`},
		"17 KiB body":    {"k", `{"restaurantId":"` + strings.Repeat("x", 17<<10) + `"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeOrder{}
			rec := place(f, tc.key, tc.body)
			if rec.Code != http.StatusBadRequest || decodeProblemCode(t, rec) != api.CodeInvalidArgument {
				t.Fatalf("status %d, body %s; want 400 invalid_argument", rec.Code, rec.Body)
			}
			if f.gotPlace != nil {
				t.Error("Order was called")
			}
		})
	}
}

func TestPlaceOrder_KeyReusedIs422(t *testing.T) {
	f := &fakeOrder{err: connect.NewError(connect.CodeAlreadyExists, nil)}
	rec := place(f, "order-1", placeBody)
	if rec.Code != http.StatusUnprocessableEntity || decodeProblemCode(t, rec) != api.CodeIdempotencyKeyReused {
		t.Fatalf("status %d, body %s; want 422 idempotency_key_reused", rec.Code, rec.Body)
	}
}

// --- get ---

func getOrder(f *fakeOrder, id string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/orders/{id}", api.NewGetOrderHandler(f, discard))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/orders/"+id, nil))
	return rec
}

func TestGetOrder_OK(t *testing.T) {
	f := &fakeOrder{order: &orderv1.GetOrderResponse{Order: sampleOrder()}}
	rec := getOrder(f, orderID)
	if rec.Code != http.StatusOK || f.gotGet.GetOrderId() != orderID || !strings.Contains(rec.Body.String(), `"status":"PENDING_PAYMENT"`) {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
}

func TestGetOrder_NotFound(t *testing.T) {
	f := &fakeOrder{err: connect.NewError(connect.CodeNotFound, nil)}
	if rec := getOrder(f, orderID); rec.Code != http.StatusNotFound || decodeProblemCode(t, rec) != api.CodeNotFound {
		t.Fatalf("status %d; want 404 not_found", rec.Code)
	}
}
