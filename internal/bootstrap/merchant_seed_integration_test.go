//go:build integration

package bootstrap

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #851: push-merchant-config --seed in MODE 2 is a seed-once importer.
// Empty DB → seed creates merchant + PSP + secret through the persistent store
// services the API uses; the PSP is armed via the runtime's store reads; a
// re-run is a no-op that never reverts store state to the manifest.
func TestMode2SeedOnceImporterFlow(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cfg := &config.Config{Env: "development", MerchantSource: config.MerchantSourceAPI, TestMode: config.CredentialPostureSandbox, Encryption: &config.EncryptionConfig{
		MasterKey: base64.StdEncoding.EncodeToString(key),
	}}

	opts, err := ResolvePushMerchantConfigOptions(cfg, true, false, false, false)
	require.NoError(t, err)
	require.Equal(t, MerchantManifestReconcileOptions{Insert: true}, opts)

	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.PSPs = map[string]PSPConfig{
		"stripe": {
			"stripe": {
				AccountID: "acct_seed_851",
				Secrets: map[string]string{
					"secret_key": "sk_test_seed_851",
				},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt

	// MODE-2 empty DB → seed.
	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, opts))

	var merchantIDText string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantIDText))
	merchantID, err := merchant.ParseID(merchantIDText)
	require.NoError(t, err)

	// PSP armed via store reads — the exact runtime path: persistent secret
	// backend + merchants.Service scoped resolution, no manifest in sight.
	backend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	require.NoError(t, err)
	svc, err := merchants.NewService(cp.Pool(), backend.Secrets, "test")
	require.NoError(t, err)
	scope, armed, err := svc.ActivePSPScope(ctx, merchantID, "stripe", "test")
	require.NoError(t, err)
	require.True(t, armed, "seeded PSP must resolve as the active account")
	require.Equal(t, "acct_seed_851", scope.AccountID)
	creds, err := svc.LoadStripeCredentials(ctx, merchantID)
	require.NoError(t, err)
	require.Equal(t, "sk_test_seed_851", creds.SecretKey)

	// Rotate the credential out of band through the store (what the API's
	// credential write lands on) — the store is now the runtime truth.
	secretName, err := merchants.PSPSecretName("stripe", "test", "acct_seed_851", "secret_key")
	require.NoError(t, err)
	_, err = backend.Secrets.Put(ctx, merchantID, secretName, "sk_test_rotated_851")
	require.NoError(t, err)

	var pspUpdatedBefore time.Time
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT updated_at FROM openrails.psps
		WHERE merchant_id = $1::uuid AND rail = 'stripe' AND account_id = 'acct_seed_851'
	`, merchantIDText).Scan(&pspUpdatedBefore))

	// Re-run of the same seed is a no-op: the manifest is never the runtime
	// source of truth after seeding.
	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, opts))

	creds, err = svc.LoadStripeCredentials(ctx, merchantID)
	require.NoError(t, err)
	require.Equal(t, "sk_test_rotated_851", creds.SecretKey, "seed re-run must not revert an out-of-band rotation to the manifest value")

	var pspCount int
	var pspUpdatedAfter time.Time
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), max(updated_at) FROM openrails.psps WHERE merchant_id = $1::uuid
	`, merchantIDText).Scan(&pspCount, &pspUpdatedAfter))
	require.Equal(t, 1, pspCount)
	require.True(t, pspUpdatedAfter.Equal(pspUpdatedBefore), "seed re-run must not touch the existing PSP row")

	var merchantCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.merchants`).Scan(&merchantCount))
	require.Equal(t, 1, merchantCount)
}
