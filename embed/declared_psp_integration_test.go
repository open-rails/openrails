//go:build integration

package embed_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestDeclaredPSPIsAttributableButNeverArmed settles the credential-less
// Runtime.DeclarePSP semantics: the account is an identity for attribution
// and links, never an armed rail. Checkout discovery does not advertise it,
// a checkout that names it is refused up front as unroutable, and a price link
// to it is stored without a provider round trip — identically through the
// embedded and standalone Clients.
func TestDeclaredPSPIsAttributableButNeverArmed(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	remote := h.StartStandalone("USD", integrationharness.WithRails(config.PSPSet{
		"ccbill": {AccountID: "999981-0000", CCBill: &config.CCBillRailConfig{Salt: "operations-local-fixture"}},
	}))
	runtime, err := embed.New(ctx, embed.Options{Options: embedded.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embedded.RiverManagedByOpenRails(),
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	local, err := runtime.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)
	mid := dbtest.TestMerchantID

	declaredKey := "stripe-declared-" + uuid.NewString()[:8]
	pspID, err := runtime.DeclarePSP(ctx, mid, embedded.PSPDeclaration{Key: declaredKey, Rail: "stripe", AccountID: "acct_declared_" + uuid.NewString()[:8]})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, pspID)
	t.Cleanup(func() {
		_, _ = h.Pool().Exec(context.Background(), `DELETE FROM openrails.psps WHERE id = $1`, pspID)
	})
	armed, err := runtime.Embedded().App().Runtime.RailConfigs.Armed(merchant.WithID(ctx, mid), "stripe")
	require.NoError(t, err)
	require.False(t, armed, "a credential-less PSP is not an armed rail")

	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": remote.Client()} {
		t.Run(name, func(t *testing.T) {
			checkout, err := client.GetCheckoutConfig(ctx)
			require.NoError(t, err)
			keys := make([]string, 0, len(checkout.PSPs))
			for _, psp := range checkout.PSPs {
				keys = append(keys, psp.Key)
			}
			require.Contains(t, keys, "ccbill", "the armed PSP is advertised")
			require.NotContains(t, keys, declaredKey, "a declared, unarmed PSP is not advertised to browsers")

			key := "declared-" + uuid.NewString()[:8]
			product, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: key, DisplayName: "Declared PSP"})
			require.NoError(t, err)
			duration := 720
			price, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, Key: key + "-price", UnitAmount: 1_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
			require.NoError(t, err)

			options, err := client.ListCheckoutRailOptions(ctx, price.ID.String())
			require.NoError(t, err)
			for _, option := range options {
				require.NotEqual(t, declaredKey, option.Selector, "an unarmed PSP is never a checkout option")
			}
			_, err = client.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{
				Customer:       openrails.CheckoutCustomerIdentity{ID: uuid.NewString(), VerifiedEmail: "buyer@example.test", Username: "buyer"},
				PriceID:        price.ID.String(),
				IdempotencyKey: uuid.NewString(),
				Payment:        openrails.CheckoutPayment{Rail: declaredKey, NameOnCard: "Test Buyer", Zip: "90210", Country: "US"},
			})
			require.ErrorIs(t, err, openrails.ErrInvalid, "a checkout naming the unarmed PSP is refused up front: %v", err)

			// The link is attribution: stored as operator-owned without a
			// provider round trip, reported linked with sync disabled.
			linked, err := client.UpdatePrice(ctx, price.ID, openrails.UpdatePriceRequest{PSPLinks: map[string]map[string]string{declaredKey: {"price_id": "price_declared_" + key}}})
			require.NoError(t, err)
			require.Equal(t, openrails.ProviderStatusLinked, linked.Providers[declaredKey].Status)
			require.Equal(t, "price_declared_"+key, linked.Providers[declaredKey].IDs["price_id"])
		})
	}
}
