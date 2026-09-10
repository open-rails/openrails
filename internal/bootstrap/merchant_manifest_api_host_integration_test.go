//go:build integration

package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/merchants"
)

// #850: the merchant manifest's api_host key is applied by
// push-merchant-config / boot reconcile — declared hosts are asserted on every
// apply, omitted hosts leave the stored value untouched, and the #734 global
// uniqueness surfaces as a loud ErrAPIHostTaken.
func TestReconcileMerchantManifestAppliesAPIHost(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	readHost := func() *string {
		var apiHost *string
		require.NoError(t, pool.QueryRow(ctx, `SELECT api_host FROM openrails.merchants WHERE slug = 'host-three'`).Scan(&apiHost))
		return apiHost
	}

	// Declared host is applied (normalized: case + port stripped).
	manifest := hostThreeMerchantManifest()
	mt := manifest.Merchants["host-three"]
	mt.APIHost = "API.Host.Example:8443"
	manifest.Merchants["host-three"] = mt
	require.NoError(t, ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true}))
	host := readHost()
	require.NotNil(t, host)
	require.Equal(t, "api.host.example", *host)

	// Omitted api_host leaves the stored value untouched (so a host assigned
	// via the merchant-admin route survives a manifest re-apply).
	require.NoError(t, ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, hostThreeMerchantManifest(), MerchantManifestReconcileOptions{Insert: true, Overwrite: true}))
	host = readHost()
	require.NotNil(t, host)
	require.Equal(t, "api.host.example", *host)

	// A changed declared host re-asserts (declarative identity, not seed-once).
	mt.APIHost = "api2.host.example"
	manifest.Merchants["host-three"] = mt
	require.NoError(t, ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true}))
	host = readHost()
	require.NotNil(t, host)
	require.Equal(t, "api2.host.example", *host)
}

func TestReconcileMerchantManifestAPIHostTakenFailsLoudly(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	first := &BillingConfig{
		Version: BootstrapManifestVersion,
		Merchants: map[string]MerchantConfig{
			"host-three": {DisplayName: "Host Three", APIHost: "api.shared.example"},
		},
	}
	require.NoError(t, ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, first, MerchantManifestReconcileOptions{Insert: true}))

	second := &BillingConfig{
		Version: BootstrapManifestVersion,
		Merchants: map[string]MerchantConfig{
			"other-app": {DisplayName: "Other App", APIHost: "api.shared.example"},
		},
	}
	err := ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, second, MerchantManifestReconcileOptions{Insert: true})
	require.Error(t, err)
	require.ErrorIs(t, err, merchants.ErrAPIHostTaken)
}

// The manifest struct path (embedded UpsertMerchantConfig hands structs
// straight to ProvisionMerchant, skipping the YAML parser) still hits the
// SetHostConfig format wall.
func TestReconcileMerchantManifestRejectsInvalidAPIHost(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	manifest := hostThreeMerchantManifest()
	mt := manifest.Merchants["host-three"]
	mt.APIHost = "https://api.host.example"
	manifest.Merchants["host-three"] = mt
	err := ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true})
	require.Error(t, err)
	require.ErrorIs(t, err, merchants.ErrInvalidAPIHost)
}
