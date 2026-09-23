//go:build integration

package embed_test

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/stretchr/testify/require"
	"testing"
)

// The same ordinary Client contract carries products, exact money and metering
// through embedded, mounted HTTP, standalone and hosted deployments.
func TestClientCatalogApplicationWorkflow(t *testing.T) {
	h := integrationharness.New(t, t.Context())
	for _, d := range clientWorkflowDeployments(t, h) {
		t.Run(d.name, func(t *testing.T) {
			client := d.client
			revision, err := client.Catalog.Revision(t.Context())
			require.NoError(t, err)
			application := &catalog.Application{SchemaVersion: 1, ApplicationID: uuid.NewString(), ExpectedRevision: &revision.Revision,
				Meters: []catalog.ApplyMeter{{Key: "calls", Aggregation: catalog.Value(catalog.AggCount), Unit: catalog.Value("call"), GroupBy: catalog.Value(map[string]string{"region": "region"})}},
				Products: []catalog.ApplyProduct{{Key: "usage", DisplayName: catalog.Value("Usage"), RateCards: catalog.Value([]catalog.RateCard{
					{Meter: "calls", Filter: map[string][]string{"region": {"west", "east"}}, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 9_007_199_254_740_993}}},
					{Price: catalog.RatePrice{Model: catalog.ModelFlat, Currency: "USD", Flat: &catalog.FlatPrice{Amount: 17}}},
				})}},
			}
			first, err := client.Catalog.Apply(t.Context(), application)
			require.NoError(t, err)
			require.False(t, first.Replayed)
			meter, err := client.GetUsageMeter(t.Context(), "calls")
			require.NoError(t, err)
			require.EqualValues(t, 9_007_199_254_740_993, meter.DefaultRateCard.Price.PerUnit.UnitAmount)
			cardID := meter.DefaultRateCard.ID
			product, err := client.Products.RetrieveByKey(t.Context(), "usage")
			require.NoError(t, err)
			title := "API edited usage"
			_, err = client.Products.Update(t.Context(), product.ID, &openrails.ProductUpdateParams{DisplayName: &title})
			require.NoError(t, err)
			replay, err := client.Catalog.Apply(t.Context(), application)
			require.NoError(t, err)
			require.True(t, replay.Replayed)
			require.Equal(t, first.AppliedRevision, replay.AppliedRevision)
			product, err = client.Products.Retrieve(t.Context(), product.ID)
			require.NoError(t, err)
			require.Equal(t, title, product.DisplayName)
			meter, err = client.GetUsageMeter(t.Context(), "calls")
			require.NoError(t, err)
			require.Equal(t, cardID, meter.DefaultRateCard.ID)
			revision, err = client.Catalog.Revision(t.Context())
			require.NoError(t, err)
			changed := &catalog.Application{SchemaVersion: 1, ApplicationID: uuid.NewString(), ExpectedRevision: &revision.Revision,
				Products: []catalog.ApplyProduct{{Key: "usage", Description: catalog.Value("Only this field changes")}}}
			_, err = client.Catalog.Apply(t.Context(), changed)
			require.NoError(t, err)
			meter, err = client.GetUsageMeter(t.Context(), "calls")
			require.NoError(t, err)
			require.Equal(t, cardID, meter.DefaultRateCard.ID, "omitted meter/rate card survives")
			require.EqualValues(t, 9_007_199_254_740_993, meter.DefaultRateCard.Price.PerUnit.UnitAmount)
		})
	}
}
