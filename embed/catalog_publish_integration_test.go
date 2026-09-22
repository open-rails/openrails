//go:build integration

package embed_test

import (
	"fmt"
	"github.com/google/uuid"
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
			_, err = client.Products.RetrieveByKey(t.Context(), "additional")
			require.ErrorIs(t, err, openrails.ErrNotFound)
			publish(openrails.CatalogPublishRequest{Insert: true})
			require.False(t, publish(all).Plan.HasChanges())
			// Existing negotiated pricing is a fixture, because its operator
			// setter is internal; the publish and usage paths below are public.
			customer, err := client.EnsureCustomer(t.Context(), (openrails.CustomerID(uuid.New())).String())
			require.NoError(t, err)
			declaration.Meters = append(declaration.Meters, catalog.Meter{Key: "protected-override", Aggregation: catalog.AggCount, GroupBy: map[string]string{"sku": "sku", "region": "region"}}, catalog.Meter{Key: "protected-history", Aggregation: catalog.AggCount})
			declaration.Products[0].RateCards = append(declaration.Products[0].RateCards, catalog.RateCard{Meter: "protected-override", Filter: map[string][]string{"region": {"west"}}, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 3}}})
			publish(all)
			pool := h.MerchantPool(d.mid.UUID())
			overrideID := uuid.New()
			_, err = pool.Exec(t.Context(), `INSERT INTO billing.catalog_rate_cards(id,merchant_id,customer_id,ordinal,meter_key,payment_term,price,allowance) VALUES($1,$2,$3,1,'protected-override','in_arrears','{"model":"per_unit","currency":"USD","per_unit":{"matrix":{"dimension":"sku","cells":{"small":{"unit_amount":"2"}}}}}'::jsonb,'{"included":20}'::jsonb)`, overrideID, d.mid.UUID(), uuid.MustParse(customer.ID))
			require.NoError(t, err)
			override := func() string {
				t.Helper()
				var value string
				require.NoError(t, pool.QueryRow(t.Context(), `SELECT row_to_json(card)::text FROM billing.catalog_rate_cards card WHERE merchant_id=$1 AND id=$2`, d.mid.UUID(), overrideID).Scan(&value))
				return value
			}
			originalOverride := override()
			// Both definitions move together. Validation against the old default
			// filter would incorrectly reject this coherent declaration.
			for i := range declaration.Meters {
				if declaration.Meters[i].Key == "protected-override" {
					declaration.Meters[i].GroupBy = map[string]string{"sku": "sku", "zone": "zone"}
				}
			}
			for i := range declaration.Products[0].RateCards {
				if declaration.Products[0].RateCards[i].Meter == "protected-override" {
					declaration.Products[0].RateCards[i].Filter = map[string][]string{"zone": {"west"}}
				}
			}
			publish(all)
			changed, err := client.GetUsageMeter(t.Context(), "protected-override")
			require.NoError(t, err)
			require.Equal(t, map[string]string{"sku": "sku", "zone": "zone"}, changed.GroupBy)
			require.Equal(t, map[string][]string{"zone": {"west"}}, changed.DefaultRateCard.Filter)
			require.Equal(t, originalOverride, override())
			require.NoError(t, client.RecordUsage(t.Context(), openrails.UsageReport{CustomerID: openrails.CustomerID(uuid.MustParse(customer.ID)), Invoker: customer.ID, Currency: "USD", EventType: "protected-history", Source: "catalog-prune", SourceID: uuid.NewString()}))
			for _, scenario := range []string{"protected-override", "protected-history", "history-edit", "card-only-prune", "override-currency", "override-dimension"} {
				t.Run(scenario, func(t *testing.T) {
					key := scenario
					if scenario == "history-edit" {
						key = "protected-history"
					}
					if scenario == "card-only-prune" || scenario == "override-currency" || scenario == "override-dimension" {
						key = "protected-override"
					}
					removeMeter := scenario == "protected-override" || scenario == "protected-history"
					removeCard := removeMeter || scenario == "card-only-prune"
					wantCode := "meter_in_use"
					if scenario == "card-only-prune" {
						wantCode = "rate_card_has_overrides"
					}
					if scenario == "override-currency" {
						wantCode = "rate_card_currency_mismatch"
					}
					if scenario == "override-dimension" {
						wantCode = "meter_rate_card_conflict"
					}
					desired := declaration
					desired.Meters = nil
					for _, meter := range declaration.Meters {
						if scenario == "override-dimension" && meter.Key == key {
							meter.GroupBy = map[string]string{"zone": "zone"}
						}
						if scenario == "history-edit" && meter.Key == key {
							meter.Unit = "must not reinterpret history"
						}
						if !removeMeter || meter.Key != key {
							desired.Meters = append(desired.Meters, meter)
						}
					}
					desired.Products = append([]catalog.Product(nil), declaration.Products...)
					desired.Products[0].DisplayName = "Must not apply before refusal"
					desired.Products[0].RateCards = nil
					for _, card := range declaration.Products[0].RateCards {
						if scenario == "override-currency" && card.Meter == key {
							card.Price.Currency = "EUR"
						}
						if !removeCard || card.Meter != key {
							desired.Products[0].RateCards = append(desired.Products[0].RateCards, card)
						}
					}
					if scenario == "override-dimension" {
						_, err := client.PublishCatalog(t.Context(), openrails.CatalogPublishRequest{Catalog: desired, Insert: true})
						require.NoError(t, err, "Insert ignores the proposed meter/default overwrite")
					}
					_, err := client.PublishCatalog(t.Context(), openrails.CatalogPublishRequest{Catalog: desired, Overwrite: true, Prune: true})
					var apiErr *openrails.StatusError
					require.ErrorAs(t, err, &apiErr)
					require.Equal(t, 409, apiErr.Status)
					require.Equal(t, wantCode, apiErr.Code)
					product, err := client.Products.RetrieveByKey(t.Context(), "usage")
					require.NoError(t, err)
					require.Equal(t, "Usage", product.DisplayName, "predictable refusal precedes product edits")
					detail, err := client.GetUsageMeter(t.Context(), key)
					require.NoError(t, err)
					if key == "protected-history" {
						require.True(t, detail.HasActivity)
					} else {
						require.EqualValues(t, 1, detail.OverrideCount)
					}
					require.Equal(t, originalOverride, override(), fmt.Sprintf("%s must preserve the exact override row", key))
				})
			}

		})
	}
}
