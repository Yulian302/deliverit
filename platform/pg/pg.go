// Package pg is the shared Postgres plumbing: a connection pool with safe
// defaults and tracing, schema migrations, and a readiness check.
//
// Startup order in a service: Connect (never fails because the DB is down),
// register ReadyCheck with the health checker, start serving. While the DB
// is unreachable the service is alive (/livez 200) but not ready (/readyz
// 503), so the orchestrator waits instead of restarting it in a loop.
package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for database/sql (goose needs it)
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// statementTimeoutMS caps every statement on pool connections. A slow or
// stuck query fails after 1s instead of holding a connection (and the
// request waiting on it) indefinitely.
const statementTimeoutMS = "1000"

// Connect builds a connection pool for dsn. It does not dial: connections are
// opened lazily on first use, so a service can start while Postgres is still
// coming up. Use ReadyCheck to find out whether the DB is reachable.
//
// Every connection gets a 1s statement_timeout, and every query becomes an
// OpenTelemetry span (child of the span in the query's context).
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx redacts the password in this error, so it is safe to log.
		return nil, fmt.Errorf("pg: parse config: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = statementTimeoutMS
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: create pool for %s/%s: %w", cfg.ConnConfig.Host, cfg.ConnConfig.Database, err)
	}
	return pool, nil
}

// ReadyCheck returns a readiness check that pings the DB through the pool.
// Register it with health.Checker.Add; the checker bounds it with a timeout.
func ReadyCheck(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		return pool.Ping(ctx)
	}
}

// Migrate applies all pending migrations in the migrations FS (goose .sql
// files at its root, e.g. 00001_create_restaurants.sql).
//
// It holds a Postgres advisory lock while migrating, so if several replicas
// run it at once they take turns: the first applies the migrations, the rest
// find nothing pending.
//
// It opens its own short-lived connection without the pool's 1s statement
// timeout, because migrations (index builds, backfills) may legitimately run
// longer than a request query.
func Migrate(ctx context.Context, dsn string, migrations fs.FS) (err error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("pg: open for migrations: %w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	// Waiters re-try the lock every 1s (goose's default is 5s, which makes a
	// waiting replica sit idle long after the lock is free), up to 300 times:
	// the same 5-minute ceiling as goose's default.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return fmt.Errorf("pg: migration lock: %w", err)
	}
	// Provider instead of goose's package-level functions: no global state,
	// so it is safe to use from parallel tests and several call sites.
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("pg: migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("pg: apply migrations: %w", err)
	}
	return nil
}
