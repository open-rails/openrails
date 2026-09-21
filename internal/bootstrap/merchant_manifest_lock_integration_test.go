//go:build integration

package bootstrap

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestMerchantManifestOwnsLockSessionAndReleasesCanceledWaiter(t *testing.T) {
	fixture := newMerchantManifestTestPool(t)
	cfg := fixture.Config().Copy()
	cfg.MaxConns, cfg.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	cp := newMerchantManifestControlPlane(t, pool)
	holderCtx, cancelHolder := context.WithCancel(t.Context())
	t.Cleanup(cancelHolder)
	release, err := lockMerchantManifestBootstrap(holderCtx, cp)
	require.NoError(t, err)
	t.Cleanup(release)
	cancelHolder() // canceled work must retain its guard until reconciliation returns
	var lockPID, workPID uint32
	require.NoError(t, fixture.QueryRow(t.Context(), `SELECT pid FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid::bigint=($1::bigint>>32) AND objid::bigint=($1::bigint&4294967295) AND granted`, merchantManifestAdvisoryLock).Scan(&lockPID))
	// This real query would starve if the guard consumed the only pool slot;
	// reusing the same physical session would make a second lock reentrant.
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&workPID))
	require.NotEqual(t, lockPID, workPID, "the guard must own a separate physical session")
	waitCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	waited := make(chan error, 1)
	go func() {
		unlock, err := lockMerchantManifestBootstrap(waitCtx, cp)
		if unlock != nil {
			unlock()
		}
		waited <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := fixture.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid::bigint=($1::bigint>>32) AND objid::bigint=($1::bigint&4294967295) AND NOT granted)`, merchantManifestAdvisoryLock).Scan(&waiting)
		return err == nil && waiting
	}, 10*time.Second, 10*time.Millisecond, "another reconciliation must wait on the owned guard")
	cancel()
	require.ErrorIs(t, <-waited, context.Canceled)
	// Cancellation of a waiter must not release the still-running holder.
	var stillHeld bool
	require.NoError(t, fixture.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted)`, lockPID).Scan(&stillHeld))
	require.True(t, stillHeld)
	release()
	require.Eventually(t, func() bool {
		var locks int
		err := fixture.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid::bigint=($1::bigint>>32) AND objid::bigint=($1::bigint&4294967295)`, merchantManifestAdvisoryLock).Scan(&locks)
		return err == nil && locks == 0
	}, 10*time.Second, 10*time.Millisecond, "neither holder nor canceled waiter may leak a session lock")
	require.NoError(t, pool.Ping(t.Context()))

	start, results := make(chan struct{}), make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- ReconcileMerchantManifestData(t.Context(), apiModeReconcileConfig(), cp, hostThreeMerchantManifest(), MerchantManifestReconcileOptions{Insert: true})
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.NoError(t, ReconcileMerchantManifestData(t.Context(), apiModeReconcileConfig(), cp, hostThreeMerchantManifest(), MerchantManifestReconcileOptions{Insert: true}))
	var count int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM billing.merchants WHERE slug='host-three'`).Scan(&count))
	require.Equal(t, 1, count, "concurrent declarations and later replay converge on one merchant")
}
