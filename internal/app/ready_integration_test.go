//go:build integration

package app

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
)

// testRuntimeDB opens a *db.DB over the shared migrated test Postgres.
func testRuntimeDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	appDB, err := db.NewWithPGXPool(pool, config.DefaultSchema)
	require.NoError(t, err)
	return appDB, dsn
}

// TestEnsureMerchantsService_ArmingFailureOutsideDev proves the #748 posture:
// a merchant-secret-store build failure is a boot ERROR outside development
// (Merchants stays nil AND the caller gets a non-nil error to propagate,
// exactly as river_register.go's addBillingWorkersToRegistry now does), while
// development keeps the original warn-and-continue.
func TestEnsureMerchantsService_ArmingFailureOutsideDev(t *testing.T) {
	appDB, _ := testRuntimeDB(t)
	ctx := context.Background()

	// A Vault login failure (bad address, nothing listening) fails
	// merchantsecrets.Build unconditionally, in every environment — unlike the
	// #667 encryption posture (which dev is deliberately ALLOWED to run
	// without), this isolates the dev-vs-non-dev ARMING gate itself.
	failingConfig := func(env string) *config.Config {
		return &config.Config{
			Env:            env,
			MerchantSource: config.MerchantSourceAPI,
			SecretBackend:  config.SecretBackendVault,
			Vault:          &config.VaultConfig{Enabled: true, Address: "http://127.0.0.1:1", AuthMethod: "token", Token: "irrelevant"},
		}
	}

	t.Run("outside development: fails boot, Merchants stays unarmed", func(t *testing.T) {
		rt := &Runtime{DB: appDB, Config: failingConfig("production")}
		err := rt.EnsureMerchantsService(ctx)
		require.Error(t, err, "arming failure outside development must fail boot (#748)")
		require.Contains(t, err.Error(), "outside development")
		require.Nil(t, rt.Merchants)
	})

	t.Run("development: warns and continues, Merchants stays unarmed but boot succeeds", func(t *testing.T) {
		rt := &Runtime{DB: appDB, Config: failingConfig("development")}
		err := rt.EnsureMerchantsService(ctx)
		require.NoError(t, err, "development keeps the original warn-and-continue")
		require.Nil(t, rt.Merchants)
	})
}

// TestReady_FullStackGreenAndNamesVaultOutage proves Runtime.Ready end to end
// (#748): green when Postgres, a Vault-backed armed merchants service, and
// River are all healthy and Redis is simply unconfigured (optional — must
// never fail readiness); it then names "merchant_secrets" when that SAME
// armed backend goes unreachable, proving a successful arm at boot is never
// mistaken for staying healthy.
func TestReady_FullStackGreenAndNamesVaultOutage(t *testing.T) {
	appDB, dsn := testRuntimeDB(t)
	ctx := context.Background()
	addr, token := vaulttest.Addr(t)

	cfg := &config.Config{
		Env:            "production",
		MerchantSource: config.MerchantSourceAPI,
		SecretBackend:  config.SecretBackendVault,
		DB:             &config.DBConfig{URL: dsn},
		Vault:          &config.VaultConfig{Enabled: true, Address: addr, AuthMethod: "token", Token: token},
	}

	rt := &Runtime{DB: appDB, Config: cfg}
	require.NoError(t, rt.EnsureMerchantsService(ctx), "arming against a healthy Vault must succeed")
	require.NotNil(t, rt.Merchants)
	require.NotNil(t, rt.MerchantSecretPing, "a Vault-backed arm must wire a live-reachability probe")

	producer, producerPool, err := buildRiverProducer(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { producerPool.Close() })
	rt.RiverProducer = producer

	deps, err := rt.Ready(ctx)
	require.NoError(t, err, "full stack green, got deps=%+v", deps)
	for _, d := range deps {
		require.Truef(t, d.Available, "%s should be available: %v", d.Name, d.Err)
		require.NotEqual(t, "garnet", d.Name, "unconfigured Redis must not even be probed (#748: required only when configured)")
	}

	// Simulate the SAME armed backend going unreachable after boot (paused
	// Vault / network partition): Ready must now name it, not report stale
	// health from arming time.
	rt.MerchantSecretPing = func(context.Context) error {
		return fmt.Errorf("vault unreachable: dial tcp: connection refused")
	}
	deps, err = rt.Ready(ctx)
	require.Error(t, err, "an unreachable Vault-backed merchant-secret store must fail Ready")
	require.Contains(t, err.Error(), "merchant_secrets")
	var found bool
	for _, d := range deps {
		if d.Name == "merchant_secrets" {
			found = true
			require.False(t, d.Available)
			require.Error(t, d.Err)
		}
	}
	require.True(t, found, "merchant_secrets must appear in the dependency detail")
}
