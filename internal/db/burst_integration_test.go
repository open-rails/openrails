//go:build integration

package db

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/tenant"
)

// TestTenantConn_BurstDoesNotWedge reproduces the #334 Phase-0 twin-pinning
// regression: 64 concurrent requests each pin a tenant connection, run a
// bun-side query AND a sqlc-side query, against a pgx pool capped far below
// the concurrency (pool_max_conns=5). With the original eager twin-pinning on
// a SHARED pool, every request held a bun connection while waiting for a pgx
// connection from the same pool — a hold-and-wait deadlock that wedged the
// server (goroutine-dump evidence in the issue tracker). With separate pools
// + lazy twin acquisition this must drain quickly.
func TestTenantConn_BurstDoesNotWedge(t *testing.T) {
	ctx := context.Background()
	superDSN, _ := startRLSContainer(t)

	// Cap the pgx pool well below the burst concurrency.
	dsn := superDSN
	if strings.Contains(dsn, "?") {
		dsn += "&pool_max_conns=5"
	} else {
		dsn += "?pool_max_conns=5"
	}
	d, err := NewDB(&config.DBConfig{URL: dsn})
	require.NoError(t, err)
	defer d.Close()

	const burst = 64
	tctx := tenant.WithID(ctx, rlsTenantA)

	errs := make([]error, burst)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = d.RunInTenantConn(tctx, func(ctx context.Context) error {
				// bun-side query on the pinned bun connection.
				var one int
				if err := d.Q(ctx).NewRaw("SELECT 1").Scan(ctx, &one); err != nil {
					return fmt.Errorf("bun side: %w", err)
				}
				// sqlc-side query — lazily acquires the pgx twin.
				var two int
				if err := d.Qx(ctx).QueryRow(ctx, "SELECT 2").Scan(&two); err != nil {
					return fmt.Errorf("pgx side: %w", err)
				}
				return nil
			})
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range errs {
		require.NoError(t, err, "request %d failed (burst starvation)", i)
	}
	// The eager twin-pinning failure mode parked requests for 4s+ per
	// timeout round; a healthy drain of 64 trivial queries is well under that.
	require.Less(t, elapsed, 20*time.Second, "burst took %s — pool starvation", elapsed)
	t.Logf("burst of %d mixed bun+pgx requests drained in %s", burst, elapsed)
}

// TestTenantConn_LazyTwinNotAcquiredWithoutUse proves the lazy twin costs
// zero pgx connections for requests that never touch a sqlc call site.
func TestTenantConn_LazyTwinNotAcquiredWithoutUse(t *testing.T) {
	ctx := context.Background()
	superDSN, _ := startRLSContainer(t)

	dsn := superDSN
	if strings.Contains(dsn, "?") {
		dsn += "&pool_max_conns=2"
	} else {
		dsn += "?pool_max_conns=2"
	}
	d, err := NewDB(&config.DBConfig{URL: dsn})
	require.NoError(t, err)
	defer d.Close()

	tctx := tenant.WithID(ctx, rlsTenantA)
	// 16 concurrent bun-only requests against a 2-conn pgx pool: if the twin
	// were eager, at most 2 could ever hold a pin and the rest would time out.
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = d.RunInTenantConn(tctx, func(ctx context.Context) error {
				var one int
				return d.Q(ctx).NewRaw("SELECT 1").Scan(ctx, &one)
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "bun-only request %d must not need a pgx connection", i)
	}
}
