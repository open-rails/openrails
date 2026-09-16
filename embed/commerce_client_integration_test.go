//go:build integration

package embed_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/embedded"
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
	local, err := embed.New(ctx, embed.Options{Options: embedded.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embedded.RiverManagedByOpenRails(),
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close(context.Background())) })
	local.Embedded().App().Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)
	productID, priceID := uuid.New(), uuid.New()
	priceKey := "commerce-" + priceID.String()
	_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Commerce fixture')`, productID, dbtest.TestMerchantID.UUID(), "commerce-"+productID.String())
	require.NoError(t, err)
	_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.prices(id,merchant_id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,$4,10000000,'USD',720,true)`, priceID, dbtest.TestMerchantID.UUID(), productID, priceKey)
	require.NoError(t, err)
	pspID := dbtest.EnsureTestPSP(ctx, t, h.Pool(), dbtest.TestMerchantID.UUID(), "ccbill")
	_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,flex_id,configuration) VALUES($1,$2,$3,$4,'{"form_name":"test-form"}')`, dbtest.TestMerchantID.UUID(), priceID, pspID, uuid.NewString())
	require.NoError(t, err)
	inprocess, err := local.Client()
	require.NoError(t, err)
	for name, client := range map[string]*openrails.Client{"embedded": inprocess, "remote": remote.Client()} {
		t.Run(name, func(t *testing.T) {
			user := uuid.NewString()
			request := openrails.CreateCheckoutSessionRequest{
				Customer: openrails.CheckoutCustomerIdentity{ID: user, VerifiedEmail: "checkout@example.test", Username: "checkout-" + uuid.NewString()[:8]},
				PriceID:  priceKey, IdempotencyKey: uuid.NewString(),
				Payment: openrails.CheckoutPayment{Rail: "ccbill", NameOnCard: "Test Buyer", Zip: "90210", Country: "US"},
			}
			options, err := client.ListCheckoutRailOptions(ctx, priceKey)
			require.NoError(t, err)
			require.NotEmpty(t, options)
			first, err := client.CreateCheckoutSession(ctx, request)
			require.NoError(t, err)
			require.NotEmpty(t, first.ID)
			require.NotNil(t, first.URL)
			require.True(t, strings.HasPrefix(*first.URL, "https://"))
			again, err := client.CreateCheckoutSession(ctx, request)
			require.NoError(t, err)
			require.Equal(t, first.ID, again.ID)
			read, err := client.GetCheckoutSession(ctx, user, first.ID)
			require.NoError(t, err)
			require.Equal(t, first.ID, read.ID)
			require.Equal(t, first.Amount, read.Amount)
			_, err = client.GetCheckoutSession(ctx, uuid.NewString(), first.ID)
			require.ErrorIs(t, err, openrails.ErrDenied)
			_, err = client.ConfirmCheckoutSession(ctx, first.ID, openrails.ConfirmCheckoutSessionRequest{CustomerID: uuid.NewString(), Payment: openrails.ConfirmPayment{Rail: "solana"}})
			require.ErrorIs(t, err, openrails.ErrDenied)
			require.NoError(t, client.Verify(openrails.WithMerchant(ctx, dbtest.TestMerchantID)))
			err = client.Verify(openrails.WithMerchant(ctx, openrails.MerchantID(uuid.New())))
			require.ErrorIs(t, err, openrails.ErrConflict, "remote and embedded must reject merchant mismatch")
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
	_, err = client.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{Customer: openrails.CheckoutCustomerIdentity{ID: uuid.NewString()}, IdempotencyKey: uuid.NewString()})
	require.ErrorIs(t, err, openrails.ErrDenied, "customer-read does not grant merchant checkout creation")
}
