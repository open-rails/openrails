//go:build integration

package merchants

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
)

// #681: a test_mode (sandbox) deployment declares environment=test provider
// accounts. The Service — built with providerEnvironment "test" — must resolve
// scoped Stripe/NMI credentials from those test rows through the real DB path
// (rail_merchant_accounts + merchant_secrets rows; no mocks), and must NOT
// resolve environment=live rows.
func TestSandboxPostureResolvesTestScopedCredentials(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	dbp := db.WrapPool(pool, "")
	store, err := NewDBSecretStore(dbp)
	require.NoError(t, err)
	svc, err := NewService(dbp, store, "test")
	require.NoError(t, err)

	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "sandbox-681", PermissionGroupID: "group-sandbox-681"})
	require.NoError(t, err)

	// Stripe: test-env account + real secret rows.
	seedRailMerchantAccount(t, svc, tn.ID, "stripe", "test", "acct_sandbox681")
	for key, value := range map[string]string{
		"secret_key":                  "sk_test_681",
		"webhook_signing_secret":      "whsec_681",
		"webhook_signing_secret_thin": "whsec_thin_681",
	} {
		name, err := PSPSecretName("stripe", "test", "acct_sandbox681", key)
		require.NoError(t, err)
		_, err = store.Put(ctx, tn.ID, name, value)
		require.NoError(t, err)
	}
	creds, err := svc.LoadStripeCredentials(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, "sk_test_681", creds.SecretKey)
	require.Equal(t, "whsec_681", creds.WebhookSigningSecret)
	require.Equal(t, "whsec_thin_681", creds.WebhookSigningThin)

	// NMI: test-env account (with tokenization settings) + webhook secret row.
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, archived, evidence)
		VALUES ($1::uuid, 'nmi', 'test', $2, false, $3::jsonb)
	`, tn.ID.String(), "681902", `{"settings":{"tokenization_key":"tok_sandbox_681"}}`)
	require.NoError(t, err)
	nmiWebhookName, err := PSPSecretName("nmi", "test", "681902", "webhook_signing_secret")
	require.NoError(t, err)
	_, err = store.Put(ctx, tn.ID, nmiWebhookName, "nmi_whsec_681")
	require.NoError(t, err)

	signing, err := svc.LoadNMIWebhookSigningSecret(ctx, tn.ID, "nmi")
	require.NoError(t, err)
	require.Equal(t, "nmi_whsec_681", signing)

	tok, err := svc.LoadNMITokenizationConfig(ctx, tn.ID, "nmi")
	require.NoError(t, err)
	require.Equal(t, "tok_sandbox_681", tok.TokenizationKey)

	// The checkout-session availability probe path.
	wantKeyName, err := PSPSecretName("nmi", "test", "681902", "security_key")
	require.NoError(t, err)
	gotKeyName, ok, err := svc.ActivePSPSecretName(ctx, tn.ID, "nmi", "test", "security_key")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, wantKeyName, gotKeyName)

	// Posture isolation: a merchant with ONLY live rows resolves nothing under
	// test posture (no silent cross-environment bleed).
	liveOnly, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "live-only-681", PermissionGroupID: "group-live-only-681"})
	require.NoError(t, err)
	seedRailMerchantAccount(t, svc, liveOnly.ID, "stripe", "live", "acct_liveonly681")
	liveSecretName, err := PSPSecretName("stripe", "live", "acct_liveonly681", "secret_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, liveOnly.ID, liveSecretName, "sk_live_681")
	require.NoError(t, err)
	liveCreds, err := svc.LoadStripeCredentials(ctx, liveOnly.ID)
	require.NoError(t, err)
	require.Empty(t, liveCreds.SecretKey)
	require.Empty(t, liveCreds.WebhookSigningSecret)
}
