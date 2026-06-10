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

// TestTenantConn_BurstDoesNotWedge guards against the #334 Phase-0 twin-pinning
// regression class: a burst of concurrent requests, each pinning a tenant
// connection and running queries, against a pool capped far below the
// concurrency (pool_max_conns=5). The original failure mode was hold-and-wait
// across two per-request acquisitions on one pool; with a single LAZY
// per-request connection, requests queue on one acquisition and the burst must
// drain quickly.
func TestTenantConn_BurstDoesNotWedge(t *testing.T) {
	ctx := context.Background()
	superDSN, _ := startRLSContainer(t)

	// Cap the pool well below the burst concurrency.
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
				// Two queries on the pinned tenant connection.
				var one int
				if err := d.Qx(ctx).QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
					return fmt.Errorf("first query: %w", err)
				}
				var two int
				if err := d.Qx(ctx).QueryRow(ctx, "SELECT 2").Scan(&two); err != nil {
					return fmt.Errorf("second query: %w", err)
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
	// The original failure mode parked requests for 4s+ per timeout round; a
	// healthy drain of 64 trivial requests is well under that.
	require.Less(t, elapsed, 20*time.Second, "burst took %s — pool starvation", elapsed)
	t.Logf("burst of %d pinned-connection requests drained in %s", burst, elapsed)
}

// TestTenantConn_LazyConnNotAcquiredWithoutUse proves the lazy pin costs zero
// pool connections for requests that never touch the database: many more
// concurrent pinned requests than pool connections succeed because nothing is
// acquired until first use.
func TestTenantConn_LazyConnNotAcquiredWithoutUse(t *testing.T) {
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
	// 16 concurrent no-op requests against a 2-conn pool: if the pin were
	// eager, at most 2 could ever hold a pin and the rest would time out.
	const n = 16
	gate := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = d.RunInTenantConn(tctx, func(ctx context.Context) error {
				<-gate // hold the pin open until every request has one
				return nil
			})
		}(i)
	}
	close(gate)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "no-op request %d must not need a pool connection", i)
	}
}
