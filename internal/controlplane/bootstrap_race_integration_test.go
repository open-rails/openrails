//go:build integration

package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestBootstrap_ConcurrentColdBoot is #844's regression guard: N app nodes
// cold-booting concurrently against one empty DB must ALL converge healthy.
// Every ensure-singleton on the boot path (root group, merchant
// permission-group, directory record) is create-or-adopt — a race loser
// re-reads the winner's row instead of dying on the singleton unique index
// (permission_groups_singleton_root_uidx / persona_instance_uidx).
func TestBootstrap_ConcurrentColdBoot(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)

	const n = 4
	// One ControlPlane per simulated node, all over the same empty DB.
	cps := make([]*ControlPlane, n)
	configs := make([]*config.Config, n)
	for i := range configs {
		configs[i] = &config.Config{Auth: &config.AuthConfig{AllowMemory: true, AllowPrivateNetworkJWKS: true, AllowMissingSenders: true, AllowEphemeralSigningKey: true, DirectPeerIP: true,
			Issuer: "https://openrails.test", KeysPath: t.TempDir(),
		}}
	}
	t.Cleanup(func() {
		for _, cp := range cps {
			if cp != nil {
				cp.Close()
			}
		}
	})

	start := make(chan struct{})
	results := make([]*BootstrapResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// AuthKit now initializes root during construction, so the
			// constructor belongs inside the cold-boot barrier too.
			cps[i], errs[i] = New(ctx, configs[i], pool)
			if errs[i] != nil {
				return
			}
			results[i], errs[i] = cps[i].Bootstrap(ctx, BootstrapOptions{
				BootstrapMerchantSlug: dbtest.TestMerchantSlug,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		require.NoErrorf(t, errs[i], "node %d bootstrap must converge, not die on the singleton index", i)
		require.NotNilf(t, results[i], "node %d result", i)
	}

	// Exactly ONE singleton root group and ONE merchant group row exist.
	var rootCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM profiles.permission_groups WHERE persona = 'root'`).Scan(&rootCount))
	require.Equal(t, 1, rootCount, "exactly one root group after %d concurrent cold boots", n)
	var merchantCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM profiles.permission_groups WHERE persona = 'merchant' AND instance_slug = $1`,
		dbtest.TestMerchantSlug).Scan(&merchantCount))
	require.Equal(t, 1, merchantCount, "exactly one merchant group")

	// Exactly one racer reports creating the merchant group; losers adopted.
	created := 0
	for i := range results {
		if results[i].MerchantGroupCreated {
			created++
		}
	}
	require.Equal(t, 1, created, "exactly one node creates; the rest adopt")

	// Every node converged on the same group id, and it is recorded on the
	// merchant directory row.
	var recorded string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT permission_group_id FROM billing.merchants WHERE slug = $1`,
		dbtest.TestMerchantSlug).Scan(&recorded))
	require.NotEmpty(t, recorded)
	for i := range results {
		require.Equalf(t, recorded, results[i].BootstrapMerchantGroupID, "node %d group id", i)
	}
}

// TestBootstrap_RootGroupRaceLoserAdopts forces the #844 loser path
// deterministically (the barrier test above only hits it probabilistically):
// an uncommitted competing root-group insert is invisible to the ensure's
// SELECT but blocks its INSERT on permission_groups_singleton_root_uidx until
// commit — exactly a race lost to another node. The loser must re-read and
// adopt the winner's row instead of failing the boot.
func TestBootstrap_RootGroupRaceLoserAdopts(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cfg := &config.Config{Auth: &config.AuthConfig{AllowMemory: true, AllowPrivateNetworkJWKS: true, AllowMissingSenders: true, AllowEphemeralSigningKey: true, DirectPeerIP: true,
		Issuer: "https://openrails.test", KeysPath: t.TempDir(),
	}}

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	var winnerPID int
	require.NoError(t, tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&winnerPID))
	_, err = tx.Exec(ctx, `INSERT INTO profiles.permission_groups (persona) VALUES ('root')`)
	require.NoError(t, err)

	var res *BootstrapResult
	done := make(chan error, 1)
	go func() {
		var berr error
		defer func() { done <- berr }()
		cp, berr := New(ctx, cfg, pool)
		if berr != nil {
			return
		}
		defer cp.Close()
		res, berr = cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug})
	}()

	// Commit the winner only once the constructor's root insert is lock-waiting
	// on it, so the loss is guaranteed, not timing-dependent.
	require.Eventually(t, func() bool {
		var waiting int
		if qerr := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))`, winnerPID).Scan(&waiting); qerr != nil {
			return false
		}
		return waiting > 0
	}, 15*time.Second, 20*time.Millisecond, "constructor root insert should block on the uncommitted winner row")
	require.NoError(t, tx.Commit(ctx))

	require.NoError(t, <-done, "race loser must adopt the winner's root row, not die on the singleton index")
	require.NotNil(t, res)

	var rootCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM profiles.permission_groups WHERE persona = 'root'`).Scan(&rootCount))
	require.Equal(t, 1, rootCount, "exactly one root row: the winner's, adopted by the loser")
}
