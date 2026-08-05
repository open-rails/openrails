//go:build integration

package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
)

// or#880 phase 3: a custodian is DECLARED ONCE and REFERENCED by every PSP
// whose gateway charges the cards it holds.
//
// The two-PSP shape is the whole point. Under phase 2 the tenant id and the
// private application key were copied into each PSP's settings, so a merchant
// running a live and a sandbox gateway against the same vault held two copies
// that could drift — and the "same" custodian silently became two.
func custodyManifest(t *testing.T) *BillingConfig {
	t.Helper()
	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.Custodians = map[string]CustodianConfig{
		"bt": {
			models.CustodianBasisTheory: {
				AccountID: "tnt_manifest_880",
				Settings: map[string]any{
					custodians.SettingPublicAPIKey:  "key_pub_880",
					custodians.SettingNetworkTokens: true,
				},
				Secrets: map[string]string{custodians.SecretAPIKey: "key_private_880"},
			},
		},
	}
	mt.PSPs = map[string]PSPConfig{
		"mobius-bt": {
			"nmi": {
				AccountID: "579145-880",
				Custodian: "bt",
				Secrets:   map[string]string{"security_key": "sk_primary_880"},
			},
		},
		"mobius-bt-backup": {
			"nmi": {
				AccountID: "579146-880",
				Custodian: "bt",
				Secrets:   map[string]string{"security_key": "sk_backup_880"},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt
	return manifest
}

func TestReconcileMerchantManifestDeclaresOneCustodianForTwoPSPs(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	require.NoError(t, ReconcileMerchantManifestData(ctx, sandboxModeReconcileConfig(), cp, custodyManifest(t), MerchantManifestReconcileOptions{Insert: true}))

	var merchantID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantID))

	// ONE custodian row, carrying the identity and the declared settings.
	var custodianID, key, kind, environment, accountID string
	var settings []byte
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT id::text, key, kind, environment, account_id, settings
		FROM openrails.custodians WHERE merchant_id = $1::uuid
	`, merchantID).Scan(&custodianID, &key, &kind, &environment, &accountID, &settings))
	require.Equal(t, "bt", key)
	require.Equal(t, models.CustodianBasisTheory, kind)
	require.Equal(t, "test", environment)
	require.Equal(t, "tnt_manifest_880", accountID)
	require.JSONEq(t, `{"public_api_key":"key_pub_880","network_tokens":true}`, string(settings))

	// BOTH PSPs reference that one row.
	rows, err := pool.Query(ctx, `
		SELECT account_id, custodian_id::text
		FROM openrails.psps WHERE merchant_id = $1::uuid ORDER BY account_id
	`, merchantID)
	require.NoError(t, err)
	defer rows.Close()
	referenced := map[string]string{}
	for rows.Next() {
		var acct, cid string
		require.NoError(t, rows.Scan(&acct, &cid))
		referenced[acct] = cid
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[string]string{"579145-880": custodianID, "579146-880": custodianID}, referenced)

	// The custodial secret is stored ONCE, under the CUSTODIAN's identity —
	// not once per PSP that charges through it.
	custodianSecret, err := merchants.CustodianSecretName(models.CustodianBasisTheory, "test", "tnt_manifest_880", custodians.SecretAPIKey)
	require.NoError(t, err)
	require.Equal(t, "custodians/basis_theory/test/tnt_manifest_880/api_key", custodianSecret)
	var stored string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT value FROM openrails.merchant_secrets WHERE merchant_id = $1::uuid AND name = $2
	`, merchantID, custodianSecret).Scan(&stored))
	require.Equal(t, "key_private_880", stored)

	var custodialSecretCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.merchant_secrets
		WHERE merchant_id = $1::uuid AND name LIKE 'custodians/%'
	`, merchantID).Scan(&custodialSecretCount))
	require.Equal(t, 1, custodialSecretCount, "one custodian means one copy of its private key")

	// Each gateway keeps its OWN security_key — custody shares the vault, not
	// the thing that charges.
	for accountID, want := range map[string]string{"579145-880": "sk_primary_880", "579146-880": "sk_backup_880"} {
		name, err := merchants.PSPSecretName("nmi", "test", accountID, "security_key")
		require.NoError(t, err)
		var value string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT value FROM openrails.merchant_secrets WHERE merchant_id = $1::uuid AND name = $2
		`, merchantID, name).Scan(&value))
		require.Equal(t, want, value)
	}

	// Re-applying is idempotent: still one custodian, still both references.
	require.NoError(t, ReconcileMerchantManifestData(ctx, sandboxModeReconcileConfig(), cp, custodyManifest(t), MerchantManifestReconcileOptions{Insert: true, Overwrite: true}))
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.custodians WHERE merchant_id = $1::uuid`, merchantID).Scan(&count))
	require.Equal(t, 1, count)
}

// A PSP that names a custodian nobody declared must FAIL the push. Arming it
// as though its gateway held the card is the wrong charge, not a degraded one.
func TestReconcileMerchantManifestRefusesUndeclaredCustodianReference(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	manifest := custodyManifest(t)
	mt := manifest.Merchants["cozy-art"]
	psp := mt.PSPs["mobius-bt"]["nmi"]
	psp.Custodian = "typo-bt"
	mt.PSPs["mobius-bt"]["nmi"] = psp
	manifest.Merchants["cozy-art"] = mt

	err := ReconcileMerchantManifestData(ctx, sandboxModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "typo-bt")
	require.Contains(t, err.Error(), "not declared")
}

// The declared kind decides which rails can charge the cards. A custodian with
// no proxy path into a rail cannot be referenced from it.
func TestReconcileMerchantManifestRefusesCustodianOnUnsupportedRail(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	manifest := custodyManifest(t)
	mt := manifest.Merchants["cozy-art"]
	mt.PSPs = map[string]PSPConfig{
		"stripe": {
			"stripe": {
				AccountID: "acct_custody_880",
				Custodian: "bt",
				Secrets:   map[string]string{"secret_key": "sk_test_880"},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt

	err := ReconcileMerchantManifestData(ctx, sandboxModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "basis_theory")
	require.Contains(t, err.Error(), "nmi")
}

// The retired phase-2 shape fails LOUDLY rather than being stored inert on a
// money path. There are no aliases (pre-launch, no consumers).
func TestReconcileMerchantManifestRefusesInlineCustodySettings(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	for name, settings := range map[string]map[string]any{
		"custodian":                {"custodian": "basis_theory"},
		"custodian_account_id":     {"custodian_account_id": "tnt_x"},
		"custodian_public_api_key": {"custodian_public_api_key": "key_pub"},
		"custodian_network_tokens": {"custodian_network_tokens": true},
		"gateway_account":          {"gateway_account": "mobius"},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := custodyManifest(t)
			mt := manifest.Merchants["cozy-art"]
			psp := mt.PSPs["mobius-bt"]["nmi"]
			psp.Settings = settings
			mt.PSPs["mobius-bt"]["nmi"] = psp
			manifest.Merchants["cozy-art"] = mt

			err := ReconcileMerchantManifestData(ctx, sandboxModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true})
			require.Error(t, err)
			require.Contains(t, err.Error(), name)
		})
	}
}

// A custodian declaration missing its required credential or public key fails
// the push — a half-declared custody arrangement is a dead checkout.
func TestReconcileMerchantManifestRefusesIncompleteCustodian(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	cases := map[string]func(*CustodianAccountConfig){
		"api_key":        func(c *CustodianAccountConfig) { delete(c.Secrets, custodians.SecretAPIKey) },
		"public_api_key": func(c *CustodianAccountConfig) { delete(c.Settings, custodians.SettingPublicAPIKey) },
		"account_id":     func(c *CustodianAccountConfig) { c.AccountID = "" },
		"unknown setting": func(c *CustodianAccountConfig) {
			c.Settings["invented_next_year"] = "x"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := custodyManifest(t)
			mt := manifest.Merchants["cozy-art"]
			entry := mt.Custodians["bt"][models.CustodianBasisTheory]
			mutate(&entry)
			mt.Custodians["bt"][models.CustodianBasisTheory] = entry
			manifest.Merchants["cozy-art"] = mt

			err := ReconcileMerchantManifestData(ctx, sandboxModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true})
			require.Error(t, err)
		})
	}
}

// An unknown vendor kind is refused by name, listing the known ones: adding a
// second custodian is a registry entry, and until it exists a typo must not
// arm anything.
func TestReconcileMerchantManifestRefusesUnknownCustodianKind(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)

	manifest := custodyManifest(t)
	mt := manifest.Merchants["cozy-art"]
	mt.Custodians = map[string]CustodianConfig{
		"hs": {"hyperswitch": {AccountID: "tnt_hs", Secrets: map[string]string{"api_key": "k"}}},
	}
	manifest.Merchants["cozy-art"] = mt

	err := ReconcileMerchantManifestData(ctx, sandboxModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "hyperswitch")
	require.Contains(t, err.Error(), models.CustodianBasisTheory)
}
