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

	stop()
	if err := waitReturn(t, done); err != nil {
		t.Fatalf("start returned %v after shutdown, want nil", err)
	}
}
