package pg_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Yulian302/deliverit/platform/pg"
)

// Review Focus 1: Postgres is not reachable when the service starts. Connect
// must still succeed (so the service starts and serves /livez), while the
// readiness check fails (so /readyz says 503 until the DB is back).
func TestReadyCheck_FailsWhenDBDown(t *testing.T) {
	ctx := context.Background()
	pool, err := pg.Connect(ctx, "postgres://u:p@127.0.0.1:1/db") // nothing listens on port 1
	if err != nil {
		t.Fatalf("Connect to an unreachable DB returned %v, want nil (it must not dial)", err)
	}
	t.Cleanup(pool.Close)

	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := pg.ReadyCheck(pool)(cctx); err == nil {
		t.Fatal("ReadyCheck returned nil for an unreachable DB")
	}
}

// The DSN holds the password; an error message that ends up in logs must not.
func TestConnect_ErrorDoesNotLeakPassword(t *testing.T) {
	_, err := pg.Connect(context.Background(), "postgres://u:s3cretpw@127.0.0.1:notaport/db")
	if err == nil {
		t.Fatal("Connect with an invalid DSN returned nil error")
	}
	if strings.Contains(err.Error(), "s3cretpw") {
		t.Errorf("error leaks the password: %v", err)
	}
}
