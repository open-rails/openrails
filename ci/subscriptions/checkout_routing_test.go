//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
)

func TestCheckoutRoutingRejectsCountry(t *testing.T) {
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newVaultWorld(t)
			ctx := t.Context()
			client := w.client[tp]
			price := w.membership("routing", 1_000_000)
			customer := w.newCustomer().cid()
			checkRouting := func(want string) {
				t.Helper()
				preview, err := client.PreviewPSPRouting(ctx, billing.PreviewPSPRoutingParams{PriceID: price.ID})
				require.NoError(t, err)
				require.NotNil(t, preview.PSP)
				require.Equal(t, want, *preview.PSP)
				order, err := client.PreviewOrder(ctx, billing.PreviewOrderParams{
					CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}},
				})
				require.NoError(t, err)
				require.NotEmpty(t, order.PaymentOptions)
				require.Equal(t, w.psp[want], order.PaymentOptions[0].PSPID)
			}

			checkRouting("stripe") // No policy: the default preference still applies.
			rules := []billing.CheckoutRoutingRule{
				{Match: billing.CheckoutRoutingMatch{Currency: "EUR"}, Prefer: []string{"stripe"}},
				{Prefer: []string{"nmi"}},
			}
			_, err := client.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{
				Settings: &billing.MerchantSettings{CheckoutRouting: &rules},
			})
			require.NoError(t, err)
			checkRouting("nmi") // USD falls through to the final rule.
			rules[0].Match.Currency = "USD"
			rules[0].Prefer = []string{"nmi"}
			rules[1].Prefer = []string{"stripe"}
			before, err := client.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{
				Settings: &billing.MerchantSettings{CheckoutRouting: &rules},
			})
			require.NoError(t, err)
			checkRouting("nmi")

			path := "openrails/merchants/" + client.MerchantID().UUID().String() + "/merchant"
			var doc map[string]any
			version := w.vault.Get(t, path, &doc)
			unsupported := []billing.CheckoutRoutingRule{
				{Match: billing.CheckoutRoutingMatch{Country: "US"}, Prefer: []string{"stripe"}},
				{Prefer: []string{"nmi"}},
			}
			_, err = client.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{
				ExpectedRevision: &before.Revision, Settings: &billing.MerchantSettings{CheckoutRouting: &unsupported},
			})
			require.Equal(t, "invalid_param", codeOf(err), "%v", err)
			require.ErrorContains(t, err, "match.country is not supported")
			require.Equal(t, version, w.vault.Get(t, path, &doc), "rejection must not write to Vault")
			after, err := client.GetMerchantConfiguration(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			checkRouting("nmi")

			for _, psp := range []string{"", "nmi"} {
				_, err := client.PreviewPSPRouting(ctx, billing.PreviewPSPRoutingParams{PriceID: price.ID, Country: " us ", PSP: psp})
				require.Equal(t, "invalid_param", codeOf(err), "%v", err)
				require.ErrorContains(t, err, "country routing is not supported")
			}

			w.editDoc("merchant", func(doc map[string]any) {
				doc["settings"].(map[string]any)["checkout_routing"] = unsupported
			})
			served, err := engine.Graph(w.rt).Runtime.MerchantConfig.Get(ctx, client.MerchantID())
			require.NoError(t, err)
			require.Contains(t, served.Rejected["merchant"], "match.country is not supported")
			after, err = client.GetMerchantConfiguration(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after, "an external edit must preserve the last valid configuration")
			checkRouting("nmi")
		})
	}
}
