package identity_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/Yulian302/deliverit/platform/identity"
)

const procedure = "/test.v1.TestService/Ping"

// server starts an in-process Connect server whose one procedure records the
// identity it finds in its context.
func server(t *testing.T) (url string, seen func() (identity.Identity, bool)) {
	t.Helper()
	var (
		got identity.Identity
		ok  bool
	)
	h := connect.NewUnaryHandler(procedure,
		func(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			got, ok = identity.FromContext(ctx)
			return connect.NewResponse(&emptypb.Empty{}), nil
		},
		connect.WithInterceptors(identity.Interceptor()),
	)
	mux := http.NewServeMux()
	mux.Handle(procedure, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, func() (identity.Identity, bool) { return got, ok }
}

func client(url string, opts ...connect.ClientOption) *connect.Client[emptypb.Empty, emptypb.Empty] {
	return connect.NewClient[emptypb.Empty, emptypb.Empty](http.DefaultClient, url+procedure, opts...)
}

func TestParseRole(t *testing.T) {
	for _, s := range []string{"customer", "restaurant", "courier", "ops"} {
		r, err := identity.ParseRole(s)
		if err != nil || string(r) != s {
			t.Errorf("ParseRole(%q) = %q, %v; want %q, nil", s, r, err, s)
		}
	}
	for _, s := range []string{"", "admin", "Customer"} {
		if _, err := identity.ParseRole(s); err == nil {
			t.Errorf("ParseRole(%q): want error", s)
		}
	}
}

func TestInterceptor_RoundTrip(t *testing.T) {
	url, seen := server(t)
	want := identity.Identity{UserID: "u1", Role: identity.RoleRestaurant, RestaurantID: "r1"}
	ctx := identity.NewContext(context.Background(), want)

	if _, err := client(url, connect.WithInterceptors(identity.Interceptor())).CallUnary(ctx, connect.NewRequest(&emptypb.Empty{})); err != nil {
		t.Fatal(err)
	}
	got, ok := seen()
	if !ok || got != want {
		t.Fatalf("server saw %+v, %v; want %+v, true", got, ok, want)
	}
}

func TestInterceptor_NoIdentity(t *testing.T) {
	url, seen := server(t)
	if _, err := client(url, connect.WithInterceptors(identity.Interceptor())).CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{})); err != nil {
		t.Fatal(err)
	}
	if got, ok := seen(); ok {
		t.Fatalf("server saw identity %+v; want none", got)
	}
}

func TestInterceptor_BadRoleIsUnauthenticated(t *testing.T) {
	url, _ := server(t)
	req := connect.NewRequest(&emptypb.Empty{})
	req.Header().Set(identity.HeaderUserID, "u1")
	req.Header().Set(identity.HeaderRole, "admin")

	_, err := client(url).CallUnary(context.Background(), req)
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("code = %v (err %v); want unauthenticated", got, err)
	}
}
