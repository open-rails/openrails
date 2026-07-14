//go:build integration

package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	authpgmigrations "github.com/open-rails/authkit/migrations/postgres"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/pkg/merchant"
)

const merchantManifestSchemaDDL = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE SCHEMA IF NOT EXISTS openrails;

CREATE TABLE IF NOT EXISTS openrails.merchants (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                TEXT NOT NULL UNIQUE,
    display_name        TEXT,
    status              TEXT NOT NULL DEFAULT 'active',
    permission_group_id     TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at          TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS openrails.merchant_configurations (
    merchant_id uuid NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    config_version bigint DEFAULT 1 NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT merchant_configurations_pkey PRIMARY KEY (merchant_id),
    CONSTRAINT merchant_configurations_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id)
);
ALTER TABLE ONLY openrails.merchant_configurations FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.merchant_configurations ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS merchant_isolation ON openrails.merchant_configurations;
CREATE POLICY merchant_isolation ON openrails.merchant_configurations
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid));

CREATE TABLE IF NOT EXISTS openrails.merchant_secrets (
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    value text NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    created_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT pk_merchant_secrets PRIMARY KEY (merchant_id, name)
);
ALTER TABLE ONLY openrails.merchant_secrets FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.merchant_secrets ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS merchant_isolation ON openrails.merchant_secrets;
CREATE POLICY merchant_isolation ON openrails.merchant_secrets
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid));

CREATE TABLE IF NOT EXISTS openrails.merchant_deks (
    merchant_id uuid NOT NULL,
    wrapped_dek bytea NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    created_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT pk_merchant_deks PRIMARY KEY (merchant_id)
);
ALTER TABLE ONLY openrails.merchant_deks FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.merchant_deks ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS merchant_isolation ON openrails.merchant_deks;
CREATE POLICY merchant_isolation ON openrails.merchant_deks
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid));

CREATE TABLE IF NOT EXISTS openrails.psps (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    environment text DEFAULT 'live' NOT NULL,
    account_id text NOT NULL,
    key text,
    archived boolean DEFAULT false NOT NULL,
    evidence jsonb,
    first_seen_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_verified_at timestamptz,
    replaced_at timestamptz,
    created_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT payment_provider_accounts_pkey PRIMARY KEY (id),
    CONSTRAINT payment_provider_accounts_nonempty CHECK (btrim(rail) <> '' AND btrim(environment) <> '' AND btrim(account_id) <> ''),
    CONSTRAINT payment_provider_accounts_environment_check CHECK (environment = ANY (ARRAY['live','test'])),
    CONSTRAINT payment_provider_accounts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE
);
ALTER TABLE ONLY openrails.psps FORCE ROW LEVEL SECURITY;
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_provider_accounts_identity ON openrails.psps (merchant_id, rail, environment, account_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_provider_accounts_global_identity ON openrails.psps (rail, environment, account_id);
CREATE INDEX IF NOT EXISTS idx_payment_provider_accounts_new_work ON openrails.psps (merchant_id, rail, environment, created_at DESC, id DESC) WHERE archived = false;
ALTER TABLE openrails.psps ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS merchant_isolation ON openrails.psps;
CREATE POLICY merchant_isolation ON openrails.psps
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid));

CREATE TABLE IF NOT EXISTS openrails.probe_verdicts (
    rail text NOT NULL,
    key_hash text NOT NULL,
    verdict text NOT NULL,
    checked_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT chk_probe_verdicts_verdict CHECK (verdict = ANY (ARRAY['live', 'simulated'])),
    CONSTRAINT probe_verdicts_pkey PRIMARY KEY (rail, key_hash)
);
`

func TestReconcileMerchantManifestEnsuresTenants(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	require.NoError(t, ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, cozyArtMerchantManifest(), MerchantManifestReconcileOptions{Insert: true}))

	var tenantID, permissionGroupID string
	require.NoError(t, pool.QueryRow(ctx, `
			SELECT id::text, permission_group_id
		  FROM openrails.merchants
		 WHERE slug = 'cozy-art'
	`).Scan(&tenantID, &permissionGroupID))

	// #567: permission_group_id now holds the merchant permission-group's internal id.
	groupID, err := cp.Core().ResolveGroupIDForSlug(ctx, controlplane.MerchantType, "cozy-art")
	require.NoError(t, err)
	require.Equal(t, groupID, permissionGroupID, "manifest bootstrap should bind the merchant directory row to its permission-group id")

	require.NoError(t, ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, cozyArtMerchantManifest(), MerchantManifestReconcileOptions{Insert: true}))

	require.NoError(t, pool.QueryRow(ctx, `
			SELECT permission_group_id
		  FROM openrails.merchants
		 WHERE slug = 'cozy-art'
	`).Scan(&permissionGroupID))
	require.Equal(t, groupID, permissionGroupID)
}

func TestReconcileMerchantManifestAppliesMerchantConfiguration(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.Profile = MerchantProfileConfig{
		DisplayName: "Cozy Art Billing",
		LogoURL:     "https://cdn.example/logo.png",
		FromEmail:   "billing@example.com",
		SupportURL:  "https://example.com/support",
	}
	mt.PSPs = map[string]PSPConfig{
		"stripe": {
			"stripe": {
				Environment: "test",
				AccountID:   "acct_test_123",
				Secrets: map[string]string{
					"secret_key": "sk_test_bootstrap",
				},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt

	require.NoError(t, ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantID))

	var displayName, logoURL, fromEmail, supportURL string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT
			config #>> '{profile,display_name}',
			config #>> '{profile,logo_url}',
			config #>> '{profile,from_email}',
			config #>> '{profile,support_url}'
		FROM openrails.merchant_configurations
		WHERE merchant_id = $1::uuid
	`, merchantID).Scan(&displayName, &logoURL, &fromEmail, &supportURL))
	require.Equal(t, "Cozy Art Billing", displayName)
	require.Equal(t, "https://cdn.example/logo.png", logoURL)
	require.Equal(t, "billing@example.com", fromEmail)
	require.Equal(t, "https://example.com/support", supportURL)

	var secretValue string
	secretName, err := merchants.PSPSecretName("stripe", "test", "acct_test_123", "secret_key")
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT value
		FROM openrails.merchant_secrets
		WHERE merchant_id = $1::uuid AND name = $2
	`, merchantID, secretName).Scan(&secretValue))
	require.Equal(t, "sk_test_bootstrap", secretValue)

	var rail, environment, accountID string
	var archived bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT rail, environment, account_id, archived
		FROM openrails.psps
		WHERE merchant_id = $1::uuid
	`, merchantID).Scan(&rail, &environment, &accountID, &archived))
	require.Equal(t, "stripe", rail)
	require.Equal(t, "test", environment)
	require.Equal(t, "acct_test_123", accountID)
	require.False(t, archived)
}

func TestReconcileMerchantManifestStoresCCBillTypedSecrets(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.PSPs = map[string]PSPConfig{
		"ccbill": {
			"ccbill": {
				Environment: "live",
				AccountID:   "900000-0000",
				Secrets: map[string]string{
					"salt":              "secret",
					"datalink_username": "merchant-user",
					"datalink_password": "merchant-pass",
				},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt

	require.NoError(t, ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantID))
	var accountID string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT account_id
		FROM openrails.psps
		WHERE merchant_id = $1::uuid AND rail = 'ccbill'
	`, merchantID).Scan(&accountID))
	require.Equal(t, "900000-0000", accountID)

	for key, want := range map[string]string{
		"salt":              "secret",
		"datalink_username": "merchant-user",
		"datalink_password": "merchant-pass",
	} {
		secretName, err := merchants.PSPSecretName("ccbill", "live", "900000-0000", key)
		require.NoError(t, err)
		var secretValue string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT value
			FROM openrails.merchant_secrets
			WHERE merchant_id = $1::uuid AND name = $2
		`, merchantID, secretName).Scan(&secretValue))
		require.Equal(t, want, secretValue)
	}
}

// #697: the manifest rejects a slash-form CCBill account_id loudly — the
// composite identity is dash-joined (clientAccnum-clientSubacc).
func TestReconcileMerchantManifestRejectsCCBillSlashAccountID(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.PSPs = map[string]PSPConfig{
		"ccbill": {
			"ccbill": {
				Environment: "live",
				AccountID:   "900000/0000",
				Secrets:     map[string]string{"salt": "secret"},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt

	err := ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "CCBill account_id uses a dash: clientAccnum-clientSubacc, e.g. 945280-0000")
}

func TestReconcileMerchantManifestStoresSolanaRailMerchantAccountConfig(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cfg := &config.Config{MerchantSource: config.MerchantSourceAPI, Encryption: &config.EncryptionConfig{
		MasterKey: base64.StdEncoding.EncodeToString(key),
	}}
	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	const (
		accountID       = "AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9"
		recipientWallet = "9hSR6S7WPtxmTojgo6GG3k4yDPecgJY292j7xrsUGWBu"
		privateKey      = "2AXDGYSE4f2sz7tvMMzyHvUfcoJmxudvdhBcmiUSo6iuCXagjUCKEQF21awZnUGxmwD4m9vGXuC3qieHXJQHAcT"
	)
	mt.PSPs = map[string]PSPConfig{
		"solana": {
			"solana": {
				Environment: "live",
				Signer:      &RailMerchantAccountSignerConfig{Mode: "local_keypair"},
				Settings: map[string]any{
					"recipient_wallet": recipientWallet,
				},
				Secrets: map[string]string{
					"private_key": privateKey,
				},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt

	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantID))

	var evidenceBytes []byte
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT evidence
		FROM openrails.psps
		WHERE merchant_id = $1::uuid AND rail = 'solana' AND environment = 'live' AND account_id = $2
	`, merchantID, accountID).Scan(&evidenceBytes))
	var evidence map[string]any
	require.NoError(t, json.Unmarshal(evidenceBytes, &evidence))
	require.Equal(t, map[string]any{"mode": "local_keypair"}, evidence["signer"])
	require.Equal(t, map[string]any{"recipient_wallet": recipientWallet}, evidence["settings"])

	secretName, err := merchants.PSPSecretName("solana", "live", accountID, "private_key")
	require.NoError(t, err)
	backend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	require.NoError(t, err)
	tid, err := merchant.ParseID(merchantID)
	require.NoError(t, err)
	sec, err := backend.Secrets.Get(ctx, tid, secretName)
	require.NoError(t, err)
	require.Equal(t, privateKey, sec.Value)
}

// #646: the merchant config round-trips — push the complete payload (profile +
// invoice + delegated-invoker windows + named test/live provider accounts), then
// dump it back into the same struct shape, with secret VALUES never emitted.
func TestMerchantConfigPushDumpRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	cfg := apiModeReconcileConfig()

	threshold := int64(75_000_000)
	floor := int64(2_000_000)
	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.DisplayName = "Cozy Art"
	mt.Profile = MerchantProfileConfig{DisplayName: "Cozy Art Billing", FromEmail: "billing@example.com"}
	mt.Invoice = &InvoiceConfig{
		CollectionThreshold:   &threshold,
		MonthlyFloor:          &floor,
		BillingPeriodBoundary: "calendar_month",
	}
	mt.DelegatedInvokerWastedSpendWindows = []BudgetWindowConfig{
		{Key: "burst", Window: "15m", Limit: 5_000_000},
		{Key: "sustained", Window: "5h", Limit: 20_000_000},
	}
	// NMI gateway "mobius": live and sandbox accounts side by side (#646).
	mt.PSPs = map[string]PSPConfig{
		"mobius": {
			"nmi": {
				Environment: "live",
				AccountID:   "579145",
				Settings: map[string]any{
					"tokenization_url": "https://secure.networkmerchants.com/token/Collect.js",
					"tokenization_key": "live-token",
				},
				Secrets: map[string]string{
					"security_key": "live-security",
				},
			},
		},
		"mobius-sandbox": {
			"nmi": {
				Environment: "test",
				AccountID:   "681902",
				Secrets: map[string]string{
					"security_key": "test-security",
				},
			},
		},
		// #697: CCBill composite identity is dash-joined (clientAccnum-clientSubacc).
		"ccbill": {
			"ccbill": {
				Environment: "live",
				AccountID:   "945280-0000",
				Secrets: map[string]string{
					"salt": "flexform-salt",
				},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt

	// First push creates the merchant; second push syncs the display name onto the
	// existing merchant row (the `found` path) — mirrors a real re-apply.
	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, MerchantManifestReconcileOptions{Insert: true, Overwrite: true}))
	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, MerchantManifestReconcileOptions{Insert: true, Overwrite: true}))

	// merchant_configurations carries the invoice policy.
	var merchantID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantID))
	parsedMerchantID, err := merchant.ParseID(merchantID)
	require.NoError(t, err)
	var boundary string
	var gotThreshold, gotFloor int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT config #>> '{billing_period_boundary}',
		       (config #>> '{collection_threshold}')::bigint,
		       (config #>> '{monthly_floor}')::bigint
		FROM openrails.merchant_configurations WHERE merchant_id = $1::uuid
	`, merchantID).Scan(&boundary, &gotThreshold, &gotFloor))
	require.Equal(t, "calendar_month", boundary)
	require.Equal(t, threshold, gotThreshold)
	require.Equal(t, floor, gotFloor)

	// The manifest PSP map key persists as the provider account key.
	var liveKey string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT key FROM openrails.psps
		WHERE merchant_id = $1::uuid AND rail = 'nmi' AND environment = 'live'
	`, merchantID).Scan(&liveKey))
	require.Equal(t, "mobius", liveKey)

	merchantSvc, err := merchants.NewService(cp.Pool(), nil, config.ExpectedProviderEnvironment(cfg.IsTestMode()))
	require.NoError(t, err)
	tokenization, err := merchantSvc.LoadNMITokenizationConfig(ctx, parsedMerchantID, "nmi")
	require.NoError(t, err)
	require.Equal(t, "live-token", tokenization.TokenizationKey)
	require.Equal(t, "https://secure.networkmerchants.com/token/Collect.js", tokenization.CollectJSURL)

	// dump the merchant back into the manifest shape.
	dumped, err := DumpMerchantConfig(ctx, cfg, cp, "cozy-art", DumpMerchantConfigOptions{})
	require.NoError(t, err)
	require.Len(t, dumped.Merchants, 1)
	d := dumped.Merchants["cozy-art"]
	require.Equal(t, "Cozy Art", d.DisplayName)
	require.Equal(t, "Cozy Art Billing", d.Profile.DisplayName)
	require.Equal(t, "billing@example.com", d.Profile.FromEmail)

	require.NotNil(t, d.Invoice)
	require.Equal(t, "calendar_month", d.Invoice.BillingPeriodBoundary)
	require.Equal(t, threshold, *d.Invoice.CollectionThreshold)
	require.Equal(t, floor, *d.Invoice.MonthlyFloor)

	require.Len(t, d.DelegatedInvokerWastedSpendWindows, 2)
	gotWindows := map[string]BudgetWindowConfig{}
	for _, w := range d.DelegatedInvokerWastedSpendWindows {
		gotWindows[w.Key] = w
	}
	require.Equal(t, "15m", gotWindows["burst"].Window)
	require.Equal(t, int64(5_000_000), gotWindows["burst"].Limit)
	require.Equal(t, "5h", gotWindows["sustained"].Window)

	require.Len(t, d.PSPs, 3)
	byEnv := map[string]ProviderRailAccountConfig{}
	var ccbillDump ProviderRailAccountConfig
	for _, account := range d.PSPs {
		require.Len(t, account, 1)
		for rail, a := range account {
			if rail == "ccbill" {
				ccbillDump = a
				continue
			}
			byEnv[a.Environment] = a
		}
	}
	// #697: the dash-form CCBill composite id survives push⇄dump verbatim.
	require.Equal(t, "945280-0000", ccbillDump.AccountID)
	require.Equal(t, "579145", byEnv["live"].AccountID)
	require.False(t, byEnv["live"].Archived)
	require.False(t, byEnv["test"].Archived)
	require.Empty(t, byEnv["live"].Secrets, "redacted dump omits secret values entirely")
	require.NotContains(t, byEnv["live"].Secrets, "tokenization_key")
	require.Equal(t, "https://secure.networkmerchants.com/token/Collect.js", byEnv["live"].Settings["tokenization_url"])
	require.Equal(t, "live-token", byEnv["live"].Settings["tokenization_key"])

	// the dump re-marshals to valid YAML that re-parses (round-trip closure).
	encoded, err := MarshalMerchantManifest(dumped)
	require.NoError(t, err)
	reparsed, err := ParseMerchantConfigManifest(encoded)
	require.NoError(t, err)
	require.NotNil(t, reparsed.Merchants["cozy-art"].Invoice)
	require.Len(t, reparsed.Merchants["cozy-art"].PSPs, 3)
	require.Equal(t, "945280-0000", reparsed.Merchants["cozy-art"].PSPs["ccbill"]["ccbill"].AccountID)
}

func TestReconcileMerchantManifestUsesConfiguredVaultSecretBackend(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	vault := newMerchantManifestVault(t)
	cfg := &config.Config{MerchantSource: config.MerchantSourceAPI, Vault: &config.VaultConfig{
		Enabled:    true,
		Address:    vault.Address,
		AuthMethod: "token",
		Token:      vault.Token,
	}}

	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.PSPs = map[string]PSPConfig{
		"stripe": {
			"stripe": {
				Environment: "test",
				AccountID:   "acct_vault_123",
				Secrets: map[string]string{
					"secret_key": "sk_test_vault_bootstrap",
				},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt

	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantIDText string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantIDText))
	secretName, err := merchants.PSPSecretName("stripe", "test", "acct_vault_123", "secret_key")
	require.NoError(t, err)
	vaultPath := "secret/openrails/merchants/cozy-art/" + secretName
	require.Equal(t, "sk_test_vault_bootstrap", readVaultKV2Value(t, vault, vaultPath))

	var dbSecretCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.merchant_secrets
		WHERE merchant_id = $1::uuid AND name = $2
	`, merchantIDText, secretName).Scan(&dbSecretCount))
	require.Equal(t, 0, dbSecretCount, "Vault-enabled bootstrap must not import provider secrets into DB merchant_secrets")

	backend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	require.NoError(t, err)
	merchantID, err := merchant.ParseID(merchantIDText)
	require.NoError(t, err)
	sec, err := backend.Secrets.Get(ctx, merchantID, secretName)
	require.NoError(t, err)
	require.Equal(t, "sk_test_vault_bootstrap", sec.Value)
}

func TestReconcileMerchantManifestUsesEncryptedDBSecretBackend(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cfg := &config.Config{MerchantSource: config.MerchantSourceAPI, Encryption: &config.EncryptionConfig{
		MasterKey: base64.StdEncoding.EncodeToString(key),
	}}

	manifest := cozyArtMerchantManifest()
	mt := manifest.Merchants["cozy-art"]
	mt.PSPs = map[string]PSPConfig{
		"stripe": {
			"stripe": {
				Environment: "test",
				AccountID:   "acct_db_123",
				Secrets: map[string]string{
					"secret_key": "sk_test_db_bootstrap",
				},
			},
		},
	}
	manifest.Merchants["cozy-art"] = mt

	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantIDText string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantIDText))
	secretName, err := merchants.PSPSecretName("stripe", "test", "acct_db_123", "secret_key")
	require.NoError(t, err)

	var storedValue string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT value
		FROM openrails.merchant_secrets
		WHERE merchant_id = $1::uuid AND name = $2
	`, merchantIDText, secretName).Scan(&storedValue))
	require.NotEqual(t, "sk_test_db_bootstrap", storedValue, "DB-backed bootstrap must store encrypted ciphertext when encryption is configured")

	backend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	require.NoError(t, err)
	merchantID, err := merchant.ParseID(merchantIDText)
	require.NoError(t, err)
	sec, err := backend.Secrets.Get(ctx, merchantID, secretName)
	require.NoError(t, err)
	require.Equal(t, "sk_test_db_bootstrap", sec.Value)
}

func TestReconcileMerchantManifestSerializesConcurrentReplicas(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest()

	start := make(chan struct{})
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := ReconcileMerchantManifestData(ctx, apiModeReconcileConfig(), cp, manifest, MerchantManifestReconcileOptions{Insert: true}); err == nil {
				successes.Add(1)
			} else {
				require.NoError(t, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 2, successes.Load())

}

type merchantManifestVault struct {
	Address string
	Token   string
}

func newMerchantManifestVault(t *testing.T) merchantManifestVault {
	t.Helper()
	ctx := context.Background()
	const token = "root"
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "hashicorp/vault:1.19",
			ExposedPorts: []string{"8200/tcp"},
			Env: map[string]string{
				"VAULT_DEV_ROOT_TOKEN_ID": token,
			},
			Cmd: []string{"server", "-dev", "-dev-root-token-id=" + token, "-dev-listen-address=0.0.0.0:8200"},
			WaitingFor: wait.ForHTTP("/v1/sys/health").
				WithPort("8200/tcp").
				WithStatusCodeMatcher(func(status int) bool {
					return status == http.StatusOK || status == http.StatusTooManyRequests
				}).
				WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "8200/tcp")
	require.NoError(t, err)
	return merchantManifestVault{
		Address: "http://" + net.JoinHostPort(host, port.Port()),
		Token:   token,
	}
}

func readVaultKV2Value(t *testing.T, vault merchantManifestVault, fullPath string) string {
	t.Helper()
	rest := strings.TrimPrefix(strings.TrimPrefix(fullPath, "secret"), "/")
	req, err := http.NewRequest(http.MethodGet, vault.Address+"/v1/secret/data/"+rest, nil)
	require.NoError(t, err)
	req.Header.Set("X-Vault-Token", vault.Token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload.Data.Data["value"]
}

func newMerchantManifestTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		pool := newExternalMerchantManifestTestPool(t, ctx, dsn)
		applyMerchantManifestTestSchema(t, ctx, pool)
		return pool
	}

	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("openrails"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		dbtest.WithPostgresLimits(),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	applyMerchantManifestTestSchema(t, ctx, pool)
	return pool
}

func newExternalMerchantManifestTestPool(t *testing.T, ctx context.Context, adminDSN string) *pgxpool.Pool {
	t.Helper()
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)

	dbName := fmt.Sprintf("openrails_tenant_manifest_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize())
	require.NoError(t, err)

	testCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	testCfg.ConnConfig.Config.Database = dbName
	pool, err := pgxpool.NewWithConfig(ctx, testCfg)
	require.NoError(t, err)

	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", dbName)
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize())
		adminPool.Close()
	})
	return pool
}

func applyMerchantManifestTestSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	entries, err := authpgmigrations.FS.ReadDir(".")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		b, rerr := authpgmigrations.FS.ReadFile(name)
		require.NoError(t, rerr)
		_, eerr := pool.Exec(ctx, string(b))
		require.NoErrorf(t, eerr, "apply authkit migration %s", name)
	}
	_, err = pool.Exec(ctx, merchantManifestSchemaDDL)
	require.NoError(t, err)
}

// apiModeReconcileConfig pins these store-semantics tests to MODE 2 (#723
// merchant_source=api): they assert persistent-backend side effects (seed-once,
// merchant_secrets rows, Vault KV) that mode 1 deliberately does not produce.
func apiModeReconcileConfig() *config.Config {
	return &config.Config{MerchantSource: config.MerchantSourceAPI}
}

func newMerchantManifestControlPlane(t *testing.T, pool *pgxpool.Pool) *controlplane.ControlPlane {
	t.Helper()
	cfg := &config.Config{
		Env: "test",
		// MintDisabled: "test" is not a dev-like env (#748: verify-only must be
		// declared outside development), and this control plane is never asked
		// to mint in these manifest-reconcile tests.
		Auth: &config.AuthConfig{Issuer: "https://openrails.test", MintDisabled: true},
	}
	cp, err := controlplane.New(context.Background(), cfg, pool)
	require.NoError(t, err)
	require.NotNil(t, cp)
	return cp
}

func cozyArtMerchantManifest() *BillingConfig {
	return &BillingConfig{
		Version: BootstrapManifestVersion,
		Merchants: map[string]MerchantConfig{
			"cozy-art": {
				DisplayName: "Cozy Art",
			},
		},
	}
}
