//go:build integration

package bootstrap

import (
	"context"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
	"testing"
)

type startupIdentityFixture struct{}

func (startupIdentityFixture) ResolveManifestPSP(_ context.Context, _ *config.Config, _ string, _ string, account ProviderRailAccountConfig, _ manifestSecretValues) (manifestProviderIdentity, error) {
	return manifestProviderIdentity{AccountID: account.AccountID}, nil
}

func TestStartupPreservesMetadataAndArchiveWhileReloadingSnapshot(t *testing.T) {
	database := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	cfg := &config.Config{SecretBackend: config.SecretBackendSnapshot, TestMode: config.CredentialPostureSandbox}
	slug := "startup-" + uuid.NewString()
	account := "acct_" + uuid.NewString()
	declared := MerchantConfig{MerchantConfig: merchantbootstrap.MerchantConfig{DisplayName: "startup name", APIHost: slug + ".example.test", Profile: MerchantProfileConfig{DisplayName: "startup profile"}, PSPs: map[string]PSPConfig{"original-key": {"stripe": {AccountID: account, Secrets: map[string]string{"secret_key": "sk_test_first", "webhook_signing_secret": "whsec_first"}}}}}}
	first := merchants.NewMemorySecretStore()
	request := ProvisionMerchantRequest{Config: cfg, Database: database, Slug: slug, Merchant: declared, SecretStore: first, Options: MerchantManifestReconcileOptions{Insert: true, IdentityResolver: startupIdentityFixture{}}}
	row, err := ProvisionMerchant(t.Context(), request)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), row.ID)
	directory, err := merchants.NewDirectoryService(database.DataPool())
	require.NoError(t, err)
	require.NoError(t, directory.SetDisplayName(ctx, row.ID, "API name"))
	require.NoError(t, directory.SetHostConfig(ctx, row.ID, slug+"-api.example.test"))
	require.NoError(t, merchantconfig.NewStore(database).Upsert(ctx, models.MerchantConfiguration{Profile: models.MerchantProfileConfiguration{DisplayName: "API profile"}}))
	require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		_, err := database.Qx(ctx).Exec(ctx, "UPDATE openrails.psps SET archived=true,key='API key' WHERE merchant_id=$1 AND account_id=$2", row.ID.UUID(), account)
		return err
	}))
	second := merchants.NewMemorySecretStore()
	declared.PSPs["original-key"]["stripe"].Secrets["secret_key"] = "sk_test_reloaded"
	request.Merchant, request.SecretStore = declared, second
	replayed, err := ProvisionMerchant(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, row.ID, replayed.ID)
	conf, _, err := merchantconfig.NewStore(database).Get(ctx)
	require.NoError(t, err)
	require.Equal(t, "API profile", conf.Profile.DisplayName)
	require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var name, host, key string
		var archived bool
		err := database.Qx(ctx).QueryRow(ctx, "SELECT m.display_name,m.api_host,p.key,p.archived FROM openrails.merchants m JOIN openrails.psps p ON p.merchant_id=m.id WHERE m.id=$1 AND p.account_id=$2", row.ID.UUID(), account).Scan(&name, &host, &key, &archived)
		require.Equal(t, "API name", name)
		require.Equal(t, slug+"-api.example.test", host)
		require.Equal(t, "API key", key)
		require.True(t, archived)
		return err
	}))
	secretName, err := merchants.PSPSecretName("stripe", "test", account, "secret_key")
	require.NoError(t, err)
	secret, err := second.Get(ctx, row.ID, secretName)
	require.NoError(t, err)
	require.Equal(t, "sk_test_reloaded", secret.Value)
	require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var count int
		err := database.Qx(ctx).QueryRow(ctx, "SELECT count(*) FROM openrails.merchant_secrets WHERE merchant_id=$1", row.ID.UUID()).Scan(&count)
		require.Zero(t, count)
		return err
	}))
}

func TestMetadataDumpIndependentOfCredentialBackend(t *testing.T) {
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	cfg := sandboxModeReconcileConfig()
	cfg.SecretBackend = config.SecretBackendSnapshot
	slug := "dump-" + uuid.NewString()
	_, err := ProvisionMerchant(t.Context(), ProvisionMerchantRequest{Config: cfg, ControlPlane: cp, Slug: slug, Merchant: MerchantConfig{MerchantConfig: merchantbootstrap.MerchantConfig{DisplayName: "Snapshot merchant"}}, Options: MerchantManifestReconcileOptions{Insert: true}})
	require.NoError(t, err)
	for _, backend := range []string{config.SecretBackendSnapshot, config.SecretBackendDB} {
		cfg.SecretBackend = backend
		// No encryption key or credential backend is needed to export metadata.
		cfg.Encryption = nil
		dumped, err := DumpMerchantConfig(t.Context(), cfg, cp, slug, DumpMerchantConfigOptions{})
		require.NoError(t, err)
		require.Contains(t, dumped.Merchants, slug)
		require.NotEmpty(t, dumped.Merchants[slug].DisplayName)
		_, err = DumpMerchantConfig(t.Context(), cfg, cp, slug, DumpMerchantConfigOptions{IncludeSecrets: true})
		require.ErrorContains(t, err, "plaintext credential export")
	}
}
