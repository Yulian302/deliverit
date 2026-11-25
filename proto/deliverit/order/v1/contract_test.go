// Package orderv1_test checks that the generated OrderService code works end
// to end: a handler mounted with the generated server code, called through
// the generated client, over HTTP/2 without TLS (h2c), the way Gateway will
// call Order. It lives next to order.proto because it tests the contract,
// not any one service.
package orderv1_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
)

// fakeOrder echoes the request back so the test can check every field made
// the round trip, and fails with InvalidArgument when lat is out of range.
type fakeOrder struct {
	orderv1connect.UnimplementedOrderServiceHandler
}

func (fakeOrder) ListRestaurants(_ context.Context, req *orderv1.ListRestaurantsRequest) (*orderv1.ListRestaurantsResponse, error) {
	if req.GetLat() > 90 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("lat out of range"))
	}
	return &orderv1.ListRestaurantsResponse{Restaurants: []*orderv1.Restaurant{{
		Id:          "r-1",
		Name:        "Echo",
		Lat:         req.GetLat(),
		Lng:         req.GetLng(),
		PrepMinutes: req.GetLimit(),
		DistanceKm:  req.GetRadiusKm(),
	}}}, nil
}

// serve mounts the generated handler on an h2c server and returns its URL.
func serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(orderv1connect.NewOrderServiceHandler(fakeOrder{}))

	srv := &http.Server{Handler: mux, Protocols: new(http.Protocols)}
	srv.Protocols.SetUnencryptedHTTP2(true)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// h2cClient speaks HTTP/2 without TLS only, so the test fails if anything
// silently falls back to HTTP/1.1.
func h2cClient() *http.Client {
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: protos}}
}

func TestContract_RoundTrip(t *testing.T) {
	for name, opts := range map[string][]connect.ClientOption{
		"connect protocol": nil,
		// Order's Connect server also speaks real gRPC: any gRPC client
		// (grpcurl, other languages) can call it.
		"grpc protocol": {connect.WithGRPC()},
	} {
		t.Run(name, func(t *testing.T) {
			client := orderv1connect.NewOrderServiceClient(h2cClient(), serve(t), opts...)

			resp, err := client.ListRestaurants(context.Background(), &orderv1.ListRestaurantsRequest{
				Lat: 50.45, Lng: 30.52, RadiusKm: 3, Limit: 5,
			})
			if err != nil {
				t.Fatalf("ListRestaurants: %v", err)
			}
			if len(resp.GetRestaurants()) != 1 {
				t.Fatalf("got %d restaurants, want 1", len(resp.GetRestaurants()))
			}
			r := resp.GetRestaurants()[0]
			if r.GetId() != "r-1" || r.GetName() != "Echo" || r.GetLat() != 50.45 || r.GetLng() != 30.52 ||
				r.GetDistanceKm() != 3 || r.GetPrepMinutes() != 5 {
				t.Errorf("round trip lost fields: %v", r)
			}
		})
	}
}

func TestContract_ErrorCodeSurvivesTheWire(t *testing.T) {
	client := orderv1connect.NewOrderServiceClient(h2cClient(), serve(t))

	_, err := client.ListRestaurants(context.Background(), &orderv1.ListRestaurantsRequest{Lat: 91})

	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("error code = %v (err %v), want invalid_argument", got, err)
	}
}
