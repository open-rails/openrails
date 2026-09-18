//go:build integration

package embed_test

import (
	"context"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/app"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// CCBill's form creation signs a redirect locally; no provider endpoint is called
// and no payment is submitted. All auth, persistence and idempotency paths are real.
func TestCommerceClientCheckoutAndTier(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	remote := h.StartStandalone("USD", integrationharness.WithRails(config.PSPSet{
		"ccbill": {AccountID: "999981-0000", CCBill: &config.CCBillRailConfig{Salt: "issue981-local-fixture"}},
	}))
	local, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close(context.Background())) })
	app.HostGraph(local).Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)
	// An armed CCBill price: product, price and its PSP binding.
	seedPrice := func(mid merchant.ID) openrails.PriceID {
		t.Helper()
		productID, priceID := uuid.New(), uuid.New()
		priceKey := "commerce-" + priceID.String()
		_, err := h.Pool().Exec(ctx, `INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Commerce fixture')`, productID, mid.UUID(), "commerce-"+productID.String())
		require.NoError(t, err)
		_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.prices(id,merchant_id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,$4,10000000,'USD',720,true)`, priceID, mid.UUID(), productID, priceKey)
		require.NoError(t, err)
		pspID := dbtest.EnsureTestPSP(ctx, t, h.Pool(), mid.UUID(), "ccbill")
		_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,flex_id,configuration) VALUES($1,$2,$3,$4,'{"form_name":"test-form"}')`, mid.UUID(), priceID, pspID, uuid.NewString())
		require.NoError(t, err)
		return openrails.PriceID(priceID)
	}
	inprocess, err := local.Client()
	require.NoError(t, err)
	// SaaS: a hosted merchant on the shared engine behind the real hosted
	// control plane, integrating with an owner-minted API key; its CCBill
	// account is armed in the shared secret store.
	hosted := h.StartHosted("USD")
	tenant := hosted.ProvisionMerchant(hosted.RegisterUser("owner"), "commerce-"+uuid.NewString()[:8])
	integrationharness.SeedPSPs(ctx, t, hosted.AppRuntime(), tenant.ID, config.PSPSet{
		"ccbill": {AccountID: "999981-0001", CCBill: &config.CCBillRailConfig{Salt: "issue981-hosted-fixture"}},
	})
	deployments := []struct {
		name    string
		client  *openrails.Client
		priceID openrails.PriceID
	}{
		{"embedded", inprocess, seedPrice(dbtest.TestMerchantID)},
		{"remote", remote.Client(), seedPrice(dbtest.TestMerchantID)},
		{"saas", tenant.Client(), seedPrice(tenant.ID)},
	}
	for _, d := range deployments {
		client, priceID := d.client, d.priceID
		t.Run(d.name, func(t *testing.T) {
			user := openrails.CustomerID(uuid.New())
			request := openrails.CreateCheckoutSessionRequest{
				Customer: openrails.CheckoutCustomerIdentity{ID: user, VerifiedEmail: "checkout@example.test", Username: "checkout-" + uuid.NewString()[:8]},
				PriceID:  priceID, IdempotencyKey: uuid.NewString(),
				Payment: openrails.CheckoutPayment{Rail: "ccbill", NameOnCard: "Test Buyer", Zip: "90210", Country: "US"},
			}
			options, err := client.ListCheckoutRailOptions(ctx, priceID)
			require.NoError(t, err)
			require.NotEmpty(t, options)
			first, err := client.CreateCheckoutSession(ctx, request)
			require.NoError(t, err)
			require.NotEmpty(t, first.ID)
			require.NotNil(t, first.URL)
			require.True(t, strings.HasPrefix(*first.URL, "https://"))
			require.False(t, first.CreatedAt.IsZero(), "created_at is an RFC3339 instant")
			require.NotNil(t, first.ExpiresAt)
			require.True(t, first.ExpiresAt.After(first.CreatedAt))
			again, err := client.CreateCheckoutSession(ctx, request)
			require.NoError(t, err)
			require.Equal(t, first.ID, again.ID)
			read, err := client.GetCheckoutSession(ctx, user, first.ID)
			require.NoError(t, err)
			require.Equal(t, first.ID, read.ID)
			require.Equal(t, first.Amount, read.Amount)
			_, err = client.GetCheckoutSession(ctx, openrails.CustomerID(uuid.New()), first.ID)
			require.ErrorIs(t, err, openrails.ErrDenied)
			_, err = client.ConfirmCheckoutSession(ctx, first.ID, openrails.ConfirmCheckoutSessionRequest{CustomerID: openrails.CustomerID(uuid.New()), Payment: openrails.ConfirmPayment{Rail: "solana"}})
			require.ErrorIs(t, err, openrails.ErrDenied)
			require.NoError(t, client.Verify(ctx))
			tier, err := client.ResolveEffectiveTier(ctx, user, "membership")
			require.NoError(t, err)
			require.Nil(t, tier, "checkout redirect alone has not bought access")
		})
	}
	bound, err := openrails.NewRemote(remote.BaseURL, openrails.WithAPIKey(remote.Token), openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)
	require.NoError(t, bound.Verify(ctx))
	wrongCredentialBinding, err := openrails.NewRemote(remote.BaseURL, openrails.WithAPIKey(remote.Token), openrails.WithMerchantID(openrails.MerchantID(uuid.New())))
	require.NoError(t, err)
	require.ErrorIs(t, wrongCredentialBinding.Verify(ctx), openrails.ErrConflict, "credential merchant cannot silently override client binding")
	readOnly := remote.MintAPIKey(dbtest.TestMerchantSlug, "commerce-reader", []string{permissions.MerchantCustomerSettingsRead})
	client, err := openrails.NewRemote(remote.BaseURL, openrails.WithAPIKey(readOnly))
	require.NoError(t, err)
	_, err = client.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{Customer: openrails.CheckoutCustomerIdentity{ID: openrails.CustomerID(uuid.New())}, IdempotencyKey: uuid.NewString()})
	require.ErrorIs(t, err, openrails.ErrDenied, "customer-read does not grant merchant checkout creation")
}
