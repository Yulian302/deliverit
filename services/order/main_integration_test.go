//go:build integration

package main

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	orderv1 "github.com/Yulian302/deliverit/gen/deliverit/order/v1"
	"github.com/Yulian302/deliverit/gen/deliverit/order/v1/orderv1connect"
)

// The full startup path against a real Postgres: migrate mode creates the
// schema, then serve mode becomes ready and shuts down cleanly.
func TestOrder_MigrateThenServeBecomesReady(t *testing.T) {
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("order"), postgres.WithUsername("order"), postgres.WithPassword("order"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	setEnv(t, freeAddr(t), dsn)
	if err := start(ctx, []string{"migrate"}, io.Discard); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('public.restaurants') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	if !exists {
		t.Fatal("restaurants table missing after migrate")
	}

	url, stop, done := startOrder(t, dsn)
	waitStatus(t, url+"/readyz", http.StatusOK)
	if err := start(ctx, []string{"healthcheck"}, io.Discard); err != nil {
		t.Fatalf("healthcheck on a ready Order = %v, want nil", err)
	}

	// The real RPC, end to end: client → h2c → handler → service → store →
	// Postgres (migrated + seeded by migrate mode above).
	client := orderv1connect.NewOrderServiceClient(h2cClient(), url)
	resp, err := client.ListRestaurants(ctx, &orderv1.ListRestaurantsRequest{Lat: 50.4501, Lng: 30.5234})
	if err != nil {
		t.Fatalf("ListRestaurants: %v", err)
	}
	if n := len(resp.GetRestaurants()); n != 10 {
		t.Errorf("got %d restaurants near Kyiv centre, want the 10 seeded", n)
	}

	stop()
	if err := waitReturn(t, done); err != nil {
		t.Fatalf("start returned %v after shutdown, want nil", err)
	}
}
