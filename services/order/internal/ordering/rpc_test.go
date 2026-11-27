package ordering_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
	"github.com/Yulian302/deliverit/platform/identity"
	"github.com/Yulian302/deliverit/services/order/internal/ordering"
)

// rpcClient serves the real handler (real Service, fake store) behind the
// identity interceptor, like Order does, and returns a client that sends the
// identity in its ctx.
func rpcClient(t *testing.T, store ordering.Store) (orderv1connect.OrderServiceClient, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	mux := http.NewServeMux()
	mux.Handle(orderv1connect.NewOrderServiceHandler(
		ordering.NewHandler(newService(store), log),
		connect.WithInterceptors(identity.Interceptor()),
	))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return orderv1connect.NewOrderServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(identity.Interceptor())), &logs
}

func as(id identity.Identity) context.Context {
	return identity.NewContext(context.Background(), id)
}

func placeRequest() *orderv1.PlaceOrderRequest {
	in := validInput()
	return &orderv1.PlaceOrderRequest{
		IdempotencyKey: in.IdempotencyKey,
		RestaurantId:   in.RestaurantID.String(),
		Lines: []*orderv1.OrderLine{
			{MenuItemId: chickenKyi.String(), Quantity: 2},
			{MenuItemId: borscht.String(), Quantity: 1},
		},
		Delivery: &orderv1.DeliveryLocation{Lat: in.Delivery.Lat, Lng: in.Delivery.Lng, Address: in.Delivery.Address},
	}
}

func TestRPC_PlaceOrder_Errors(t *testing.T) {
	courier := identity.Identity{UserID: uuid.NewString(), Role: identity.RoleCourier}
	cases := map[string]struct {
		ctx   context.Context
		store *fakeStore
		req   func(*orderv1.PlaceOrderRequest)
		want  connect.Code
	}{
		"no identity":        {context.Background(), &fakeStore{}, nil, connect.CodeUnauthenticated},
		"courier":            {as(courier), &fakeStore{}, nil, connect.CodePermissionDenied},
		"key reused":         {as(olena), &fakeStore{err: ordering.ErrKeyReused}, nil, connect.CodeAlreadyExists},
		"invalid from store": {as(olena), &fakeStore{err: ordering.ErrInvalidArgument}, nil, connect.CodeInvalidArgument},
		"bad restaurant id":  {as(olena), &fakeStore{}, func(r *orderv1.PlaceOrderRequest) { r.RestaurantId = "x" }, connect.CodeInvalidArgument},
		"bad menu item id":   {as(olena), &fakeStore{}, func(r *orderv1.PlaceOrderRequest) { r.Lines[0].MenuItemId = "x" }, connect.CodeInvalidArgument},
		"no delivery":        {as(olena), &fakeStore{}, func(r *orderv1.PlaceOrderRequest) { r.Delivery = nil }, connect.CodeInvalidArgument},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := rpcClient(t, tc.store)
			req := placeRequest()
			if tc.req != nil {
				tc.req(req)
			}
			_, err := client.PlaceOrder(tc.ctx, req)
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("code = %v (err %v); want %v", got, err, tc.want)
			}
		})
	}
}

func TestRPC_InternalErrorsAreHidden(t *testing.T) {
	client, logs := rpcClient(t, &fakeStore{err: errors.New("db down: secret host")})
	_, err := client.GetOrder(as(olena), &orderv1.GetOrderRequest{OrderId: uuid.NewString()})
	if connect.CodeOf(err) != connect.CodeInternal || strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v; want internal without details", err)
	}
	if !strings.Contains(logs.String(), "db down") {
		t.Error("the real error was not logged")
	}
}

func TestRPC_GetOrder_NotFound(t *testing.T) {
	client, _ := rpcClient(t, &fakeStore{err: ordering.ErrNotFound})
	_, err := client.GetOrder(as(olena), &orderv1.GetOrderRequest{OrderId: uuid.NewString()})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("err = %v; want not_found", err)
	}
}

func TestRPC_PlaceOrder_MapsOrder(t *testing.T) {
	order := ordering.Order{
		ID: uuid.Must(uuid.NewV7()), CustomerID: olenaID, RestaurantID: khreshchat,
		Status:     ordering.StatusPendingPayment,
		Items:      []ordering.Item{{MenuItemID: chickenKyi, Name: "Chicken Kyiv", UnitPriceMinor: 26500, Quantity: 2}},
		TotalMinor: 53000, Currency: "UAH",
		Delivery:  ordering.Delivery{Lat: 50.45, Lng: 30.52, Address: "Khreshchatyk 1"},
		CreatedAt: t0, Version: 1,
	}
	store := &fakeStore{order: order, replayed: true}
	client, _ := rpcClient(t, store)
	resp, err := client.PlaceOrder(as(olena), placeRequest())
	if err != nil {
		t.Fatal(err)
	}
	o := resp.GetOrder()
	if !resp.GetReplayed() || o.GetId() != order.ID.String() || o.GetCustomerId() != olenaID.String() ||
		o.GetStatus() != orderv1.OrderStatus_ORDER_STATUS_PENDING_PAYMENT || o.GetTotalMinor() != 53000 ||
		o.GetCurrency() != "UAH" || !o.GetCreatedAt().AsTime().Equal(t0) || o.GetVersion() != 1 ||
		o.GetDelivery().GetAddress() != "Khreshchatyk 1" {
		t.Errorf("response = %v", resp)
	}
	if it := o.GetItems(); len(it) != 1 || it[0].GetName() != "Chicken Kyiv" || it[0].GetUnitPriceMinor() != 26500 || it[0].GetQuantity() != 2 {
		t.Errorf("items = %v", o.GetItems())
	}
	// And the request reached the service intact.
	in := store.placement.Input
	if in.IdempotencyKey != "order-1" || len(in.Lines) != 2 || in.Lines[0].MenuItemID != chickenKyi || in.Lines[0].Quantity != 2 || in.Delivery.Address != "Khreshchatyk 1, flat 5" {
		t.Errorf("service got %+v", in)
	}
}
