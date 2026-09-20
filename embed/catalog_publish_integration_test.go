//go:build integration

package embed_test

import (
	"testing"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/stretchr/testify/require"
)

// One application program sends the complete billing declaration through every
// Client deployment, without internal executor callbacks or a second HTTP client.
func TestClientCatalogPublishingWorkflow(t *testing.T) {
	h := integrationharness.New(t, t.Context())
	for _, d := range clientWorkflowDeployments(t, h) {
		t.Run(d.name, func(t *testing.T) {
			client := d.client
			declaration := catalog.Manifest{Version: catalog.SupportedVersion,
				Meters: []catalog.Meter{
					{Key: "calls", Aggregation: catalog.AggCount, Unit: "call", GroupBy: map[string]string{"region": "region"}},
					{Key: "unused", Aggregation: catalog.AggCount},
				},
				Products: []catalog.Product{{Key: "usage", DisplayName: "Usage", RateCards: []catalog.RateCard{
					{Meter: "calls", Filter: map[string][]string{"region": {"west", "east"}}, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 9_007_199_254_740_993}}},
					{Price: catalog.RatePrice{Model: catalog.ModelFlat, Currency: "USD", Flat: &catalog.FlatPrice{Amount: 17}}},
				}}},
			}
			publish := func(opts openrails.CatalogPublishRequest) *openrails.CatalogPublishResponse {
				t.Helper()
				opts.Catalog = declaration
				out, err := client.PublishCatalog(t.Context(), opts)
				require.NoError(t, err)
				require.NotNil(t, out.Plan)
				return out
			}
			all := openrails.CatalogPublishRequest{Insert: true, Overwrite: true, Prune: true}
			preview := publish(openrails.CatalogPublishRequest{})
			require.Nil(t, preview.Result)
			require.True(t, preview.Plan.MetersChanged)
			require.True(t, preview.Plan.RateCardsChanged)
			rows, n, err := client.ListUsageMeters(t.Context(), openrails.PageOptions{})
			require.NoError(t, err)
			require.Empty(t, rows)
			require.Zero(t, n)
			publish(openrails.CatalogPublishRequest{Insert: true})
			require.False(t, publish(all).Plan.HasChanges(), "second publish is quiet")
			meter, err := client.GetUsageMeter(t.Context(), "calls")
			require.NoError(t, err)
			require.EqualValues(t, 9_007_199_254_740_993, meter.DefaultRateCard.Price.PerUnit.UnitAmount)
			cardID := meter.DefaultRateCard.ID

			// Explicit defaults, map/filter iteration order and meter ordering do not
			// change the stored meaning or spuriously rewrite a rate card.
			declaration.Meters[0].EventType = "calls"
			declaration.Products[0].RateCards[0].PaymentTerm = catalog.PaymentInArrears
			declaration.Products[0].RateCards[0].Price.PerUnit.DivideBy = 1
			declaration.Products[0].RateCards[0].Price.PerUnit.Round = catalog.RoundHalfUp
			declaration.Products[0].RateCards[0].Allowance = &catalog.Allowance{}
			declaration.Products[0].RateCards[0].Filter["region"] = []string{"east", "west"}
			declaration.Meters[0], declaration.Meters[1] = declaration.Meters[1], declaration.Meters[0]
			require.False(t, publish(all).Plan.HasChanges())
			meter, err = client.GetUsageMeter(t.Context(), "calls")
			require.NoError(t, err)
			require.Equal(t, cardID, meter.DefaultRateCard.ID)

			declaration.Meters[1].Unit = "request"
			preview = publish(openrails.CatalogPublishRequest{})
			require.True(t, preview.Plan.MetersChanged)
			require.False(t, preview.Plan.RateCardsChanged)
			publish(openrails.CatalogPublishRequest{Insert: true})
			meter, err = client.GetUsageMeter(t.Context(), "calls")
			require.NoError(t, err)
			require.Equal(t, "call", meter.Unit)
			publish(openrails.CatalogPublishRequest{Overwrite: true})
			meter, err = client.GetUsageMeter(t.Context(), "calls")
			require.NoError(t, err)
			require.Equal(t, "request", meter.Unit)
			require.Equal(t, cardID, meter.DefaultRateCard.ID)
			require.False(t, publish(all).Plan.HasChanges())

			declaration.Products[0].RateCards[0].Price.PerUnit.UnitAmount = 23
			preview = publish(openrails.CatalogPublishRequest{})
			require.False(t, preview.Plan.MetersChanged)
			require.True(t, preview.Plan.RateCardsChanged)
			publish(openrails.CatalogPublishRequest{Insert: true})
			meter, err = client.GetUsageMeter(t.Context(), "calls")
			require.NoError(t, err)
			require.EqualValues(t, 9_007_199_254_740_993, meter.DefaultRateCard.Price.PerUnit.UnitAmount)
			publish(openrails.CatalogPublishRequest{Overwrite: true})
			meter, err = client.GetUsageMeter(t.Context(), "calls")
			require.NoError(t, err)
			require.EqualValues(t, 23, meter.DefaultRateCard.Price.PerUnit.UnitAmount)
			require.False(t, publish(all).Plan.HasChanges())

			// Omissions are removals only with Prune; overwriting a product never
			// implicitly prunes an unrelated meter or the product's omitted flat fee.
			declaration.Meters = declaration.Meters[1:]
			declaration.Products[0].RateCards = declaration.Products[0].RateCards[:1]
			publish(openrails.CatalogPublishRequest{Overwrite: true})
			_, err = client.GetUsageMeter(t.Context(), "unused")
			require.NoError(t, err)
			preview = publish(openrails.CatalogPublishRequest{})
			require.True(t, preview.Plan.MetersChanged)
			require.True(t, preview.Plan.RateCardsChanged)
			publish(openrails.CatalogPublishRequest{Prune: true})
			_, err = client.GetUsageMeter(t.Context(), "unused")
			require.ErrorIs(t, err, openrails.ErrNotFound)
			require.False(t, publish(all).Plan.HasChanges())

			// Overwrite/Prune do not create an omitted product's new billing definition.
			declaration.Products = append(declaration.Products, catalog.Product{Key: "additional", DisplayName: "Additional", RateCards: []catalog.RateCard{{Price: catalog.RatePrice{Model: catalog.ModelFlat, Currency: "USD", Flat: &catalog.FlatPrice{Amount: 5}}}}})
			publish(openrails.CatalogPublishRequest{Overwrite: true, Prune: true})
			_, err = client.GetProductByKey(t.Context(), "additional")
			require.ErrorIs(t, err, openrails.ErrNotFound)
			publish(openrails.CatalogPublishRequest{Insert: true})
			require.False(t, publish(all).Plan.HasChanges())
		})
	}
}
