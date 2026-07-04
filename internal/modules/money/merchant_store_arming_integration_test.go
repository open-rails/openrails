//go:build integration

package money_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// This file is the store-arming seam for the live rail tests (#699): rail
// credentials reach the charge path the way production arms them — a
// rail_merchant_accounts row plus a scoped secret in the merchant-secrets
// store, resolved back through the production resolvers — never a raw env
// value injected into a boot-plane RailMerchantAccountSet.

// merchantsServiceForTest builds the production merchants.Service over the
// shared test DB with the production DB-backed secret store (real
// openrails.merchant_secrets rows), in the test-environment posture the live
// tests run under (TEST_MODE=sandbox).
func merchantsServiceForTest(t *testing.T, dbi *db.DB) *merchants.Service {
	t.Helper()
	store, err := merchants.NewDBSecretStore(dbi.DataPool())
	require.NoError(t, err)
	svc, err := merchants.NewService(dbi.DataPool(), store, config.ExpectedProviderEnvironment(true))
	require.NoError(t, err)
	return svc
}

// seedRailMerchantAccountSecrets arms one rail for the shared test merchant
// exactly the way the merchant manifest does (bootstrap
// reconcileManifestRailMerchantAccount): each secret is Put under its canonical
// scoped name (merchants.RailMerchantAccountSecretName) and the
// operator-declared account row is upserted via gen.UpsertRailMerchantAccount
// in a merchant-scoped connection. Cleanup removes both.
func seedRailMerchantAccountSecrets(t *testing.T, dbi *db.DB, svc *merchants.Service, rail, accountID string, secrets map[string]string) {
	t.Helper()
	ctx := context.Background()
	env := config.ExpectedProviderEnvironment(true)
	for key, value := range secrets {
		name, err := merchants.RailMerchantAccountSecretName(rail, env, accountID, key)
		require.NoError(t, err)
		_, err = svc.Secrets().Put(ctx, dbtest.TestMerchantID, name, value)
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = svc.Secrets().Delete(context.Background(), dbtest.TestMerchantID, name)
		})
	}
	mctx := merchant.WithID(ctx, dbtest.TestMerchantID)
	require.NoError(t, dbi.RunInMerchantConn(mctx, func(ctx context.Context) error {
		_, err := dbi.Gen(ctx).UpsertRailMerchantAccount(ctx, gen.UpsertRailMerchantAccountParams{
			MerchantID:  dbtest.TestMerchantID.UUID(),
			Rail:        rail,
			Environment: &env,
			AccountID:   accountID,
		})
		return err
	}))
	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(context.Background(),
			`DELETE FROM openrails.rail_merchant_accounts WHERE merchant_id = $1 AND rail = $2 AND environment = $3 AND account_id = $4`,
			dbtest.TestMerchantID.UUID(), rail, env, accountID)
	})
}

// TestRailCredentialStoreArming_ProductionResolutionPath proves the arming
// mechanics WITHOUT any provider network, so the seam stays covered even when
// the live-gated jobs skip: seed via the manifest-path helper, then require the
// production resolvers to return the seeded values —
//   - Stripe: merchants.Service.LoadStripeCredentials (active account scope ->
//     scoped secret name -> store Get; the webhook/pull-plane resolver), and the
//     RequireStripeSecretKey gate every StripeService call site goes through;
//   - NMI: ActiveRailMerchantAccountSecretName + Secrets().Get (checkout's
//     merchant_rail_secrets.go seam).
func TestRailCredentialStoreArming_ProductionResolutionPath(t *testing.T) {
	_, dbi, _, _, _, ctx := moneyInEnvWithDB(t)
	msvc := merchantsServiceForTest(t, dbi)
	sfx := uuid.NewString()[:8]

	// Stripe leg.
	stripeKey := "sk_test_store_arming_" + sfx
	stripeAccount := "acct_arming" + sfx
	seedRailMerchantAccountSecrets(t, dbi, msvc, string(models.RailStripe), stripeAccount, map[string]string{"secret_key": stripeKey})
	creds, err := msvc.LoadStripeCredentials(ctx, dbtest.TestMerchantID)
	require.NoError(t, err)
	require.Equal(t, stripeKey, creds.SecretKey, "LoadStripeCredentials must resolve the seeded scoped secret")

	stripeSvc := &subscriptions.StripeService{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox},
		Rails: config.RailMerchantAccountSet{
			"stripe": {Rail: models.RailStripe, AccountID: stripeAccount, Stripe: &config.StripeRailConfig{SecretKey: creds.SecretKey}},
		},
	}
	_, gateKey, err := subscriptions.RequireStripeSecretKey(stripeSvc.Rails)
	require.NoError(t, err)
	require.Equal(t, stripeKey, gateKey, "the charge service must carry the store-resolved key")

	// NMI leg (checkout's resolution seam).
	nmiKey := "store-arming-security-key-" + sfx
	nmiAccount := "arming-" + sfx
	seedRailMerchantAccountSecrets(t, dbi, msvc, string(models.RailNMI), nmiAccount, map[string]string{"security_key": nmiKey})
	name, found, err := msvc.ActiveRailMerchantAccountSecretName(ctx, dbtest.TestMerchantID, string(models.RailNMI), config.ExpectedProviderEnvironment(true), "security_key")
	require.NoError(t, err)
	require.True(t, found, "seeded NMI rail account must resolve to a scoped secret name")
	sec, err := msvc.Secrets().Get(ctx, dbtest.TestMerchantID, name)
	require.NoError(t, err)
	require.Equal(t, nmiKey, sec.Value, "store Get must return the seeded NMI credential")
}
