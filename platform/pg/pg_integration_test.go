//go:build integration

package pg_test

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Yulian302/deliverit/platform/pg"
)

// adminDSN points at the shared test container's default database. Tests
// never use it directly; each one gets a fresh database from newDB.
var adminDSN string

// TestMain starts one Postgres container for the whole package: starting a
// container per test would cost seconds each.
func TestMain(m *testing.M) {
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		log.Fatalf("start postgres container: %v", err)
	}
	adminDSN, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("connection string: %v", err)
	}

	code := m.Run()
	if err := testcontainers.TerminateContainer(ctr); err != nil {
		log.Printf("terminate container: %v", err)
	}
	os.Exit(code)
}

// newDB creates an empty database for one test and returns its DSN, so tests
// can't see each other's tables or goose version rows.
func newDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	name := "t_" + uuid.NewString()[:8]

	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }() // test setup; nothing useful to do on a close error
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func connect(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pg.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func tableExists(t *testing.T, dsn, table string) bool {
	t.Helper()
	var ok bool
	err := connect(t, dsn).QueryRow(context.Background(),
		"SELECT to_regclass($1) IS NOT NULL", "public."+table).Scan(&ok)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestMigrate_AppliesAndIsIdempotent(t *testing.T) {
	dsn := newDB(t)
	migrations := fstest.MapFS{
		"00001_t.sql": {Data: []byte("-- +goose Up\nCREATE TABLE t(id int);\n-- +goose Down\nDROP TABLE t;\n")},
	}

	for i := range 2 { // the second run must be a no-op, not "table already exists"
		if err := pg.Migrate(context.Background(), dsn, migrations); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}
	if !tableExists(t, dsn, "t") {
		t.Error("table t does not exist after Migrate")
	}
}

// Several replicas may run migrations at once (e.g. two containers starting
// together). The advisory lock must make them take turns: one applies the
// migration, the others find it already applied. Without the lock, two
// would run the same CREATE TABLE and one would fail.
func TestMigrate_ConcurrentRunsApplyOnce(t *testing.T) {
	dsn := newDB(t)
	migrations := fstest.MapFS{
		"00001_init.sql": {Data: []byte("-- +goose Up\nSELECT 1;\n-- +goose Down\nSELECT 1;\n")},
	}
	// Apply a first migration alone, so goose's version table already
	// exists. Otherwise the runs race on creating that table, goose retries
	// with a back-off, and the back-off hides a missing lock.
	if err := pg.Migrate(context.Background(), dsn, migrations); err != nil {
		t.Fatal(err)
	}
	// pg_sleep widens the window in which an unlocked run would also see
	// 00002 as pending and run the same CREATE TABLE.
	migrations["00002_c.sql"] = &fstest.MapFile{
		Data: []byte("-- +goose Up\nCREATE TABLE c(id int);\nSELECT pg_sleep(0.3);\n-- +goose Down\nDROP TABLE c;\n"),
	}

	const runs = 4
	errs := make([]error, runs)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Go(func() { errs[i] = pg.Migrate(context.Background(), dsn, migrations) })
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate %d: %v", i, err)
		}
	}
	if !tableExists(t, dsn, "c") {
		t.Error("table c does not exist after Migrate")
	}
}

func TestConnect_StatementTimeout(t *testing.T) {
	db := connect(t, newDB(t))

	start := time.Now()
	_, err := db.Exec(context.Background(), "SELECT pg_sleep(2)")

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" { // 57014 = query_canceled
		t.Fatalf("err = %v, want a Postgres error with code 57014 (statement timeout)", err)
	}
	if took := time.Since(start); took > 1900*time.Millisecond {
		t.Errorf("query ran %v, want it cut off at ~1s", took)
	}
}

func TestReadyCheck_PassesWhenDBUp(t *testing.T) {
	pool := connect(t, newDB(t))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := pg.ReadyCheck(pool)(ctx); err != nil {
		t.Fatalf("ReadyCheck against a running DB: %v", err)
	}
}
