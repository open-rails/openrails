//go:build integration

package app

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// TestProbeVerdictCacheRoundtrip exercises the #348 probe-cooldown persistence
// exactly as the boot path uses it: the unprivileged openrails_app role on a
// non-merchant-pinned connection (openrails.probe_verdicts is RLS-exempt by
// design — the probe runs before any merchant context exists).
func TestProbeVerdictCacheRoundtrip(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	appDB, err := db.NewWithPGXPool(pool, "") // default schema (shared harness)
	require.NoError(t, err)

	keyHash := probeKeyHash("integration-test-key")

	// Miss before any store.
	_, _, ok := lookupProbeVerdict(appDB, "mobius", keyHash)
	assert.False(t, ok, "no verdict cached yet")

	// Store + read back: fresh simulated -> skip probe.
	storeProbeVerdict(appDB, "mobius", keyHash, probeVerdictSimulated)
	verdict, checkedAt, ok := lookupProbeVerdict(appDB, "mobius", keyHash)
	require.True(t, ok)
	assert.Equal(t, probeVerdictSimulated, verdict)
	assert.WithinDuration(t, time.Now(), checkedAt, time.Minute)
	assert.Equal(t, probeCacheSkipProbe, probeCacheDecision(verdict, checkedAt, time.Now()))

	// Upsert flips the verdict in place: fresh live -> refuse boot from cache.
	storeProbeVerdict(appDB, "mobius", keyHash, probeVerdictLive)
	verdict, checkedAt, ok = lookupProbeVerdict(appDB, "mobius", keyHash)
	require.True(t, ok)
	assert.Equal(t, probeVerdictLive, verdict)
	assert.Equal(t, probeCacheRefuseBoot, probeCacheDecision(verdict, checkedAt, time.Now()))

	// A rotated key is a different cache identity: always re-probes.
	_, _, ok = lookupProbeVerdict(appDB, "mobius", probeKeyHash("rotated-key"))
	assert.False(t, ok, "key rotation must miss the cache")

	// Same key under another provider name is independent.
	_, _, ok = lookupProbeVerdict(appDB, "acme", keyHash)
	assert.False(t, ok)

	// Degradation: a nil DB behaves like a miss and a no-op store.
	_, _, ok = lookupProbeVerdict(nil, "mobius", keyHash)
	assert.False(t, ok)
	storeProbeVerdict(nil, "mobius", keyHash, probeVerdictSimulated) // must not panic
}
