//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules/syntax"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func publicSale(t *testing.T, a *authorityWorld, key string, amount int64, hours *int) (*billing.Product, *billing.Price) {
	t.Helper()
	product, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: "Public contract " + key, Ownership: catalog.OwnershipConsumable, Entitlements: []string{"library:read"}})
	require.NoError(t, err)
	price, err := a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, UnitAmount: amount, Currency: "USD", AccessDurationHours: hours})
	require.NoError(t, err)
	return product, price
}

func publicPurchase(t *testing.T, a *authorityWorld, p *providerLedger, price billing.PriceID, quantity int, total int64, key string) billing.Order {
	t.Helper()
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price, Quantity: ptr(quantity)}}, ExpectedTotal: ptr(total), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status)
	require.Equal(t, billing.CustomerID(uuid.MustParse(a.alice.ID)), order.CustomerID)
	require.Equal(t, total, order.Total)
	require.NotNil(t, order.Payment.PaymentID)
	return order
}

func TestPublicCatalog(t *testing.T) {
	t.Run("OR-J05-S16/quantity-only-price-versions-and-document-apply", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		product, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "team", DisplayName: "Team"})
		require.NoError(t, err)
		input := billing.CreatePriceParams{ProductID: product.ID, Key: "seats", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(720), Quantity: &catalog.Quantity{Min: 1, Max: 10}}
		status, raw := a.request(t, http.MethodPost, "/v1/catalog/prices", a.alphaKey.Secret, input)
		require.Equal(t, http.StatusCreated, status, string(raw))
		var first billing.Price
		require.NoError(t, json.Unmarshal(raw, &first))
		require.EqualValues(t, 1, first.Version)
		require.False(t, first.Archived)
		require.Equal(t, input.Quantity, first.Quantity)

		input.Quantity = &catalog.Quantity{Min: 1, Max: 50}
		status, raw = a.request(t, http.MethodPost, "/v1/catalog/prices", a.alphaKey.Secret, input)
		require.Equal(t, http.StatusCreated, status, string(raw))
		var second billing.Price
		require.NoError(t, json.Unmarshal(raw, &second))
		require.NotEqual(t, first.ID, second.ID)
		require.EqualValues(t, 2, second.Version)
		require.False(t, second.Archived)
		require.Equal(t, input.Quantity, second.Quantity)
		archived, err := a.alphaRemote.GetPrice(t.Context(), first.ID)
		require.NoError(t, err)
		require.True(t, archived.Archived)
		require.Equal(t, first.Quantity, archived.Quantity)

		// A file-owned price exercises the same change without an edit-ownership
		// conflict; another product's rename must commit in the same document.
		document, err := catalog.ParseApplicationJSON([]byte(`{"schema_version":1,"products":{"file-team":{"display_name":"File team","prices":{"seats":{"amount":"10.00 USD","billing_interval":"30 days","quantity":{"min":1,"max":10}}}},"other":{"display_name":"Original name"}}}`))
		require.NoError(t, err)
		receipt, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.Empty(t, receipt.Conflicts)
		require.Empty(t, receipt.Warnings)
		prices, err := a.alphaRemote.ListPrices(t.Context(), billing.PriceListParams{ProductKey: "file-team", Key: "seats"})
		require.NoError(t, err)
		require.Len(t, prices.Items, 1)
		original := prices.Items[0]
		require.EqualValues(t, 1, original.Version)
		require.Equal(t, &catalog.Quantity{Min: 1, Max: 10}, original.Quantity)
		team := document.Products["file-team"]
		seats := team.Prices["seats"]
		seats.Quantity = catalog.Value(catalog.Quantity{Min: 1, Max: 50})
		team.Prices["seats"] = seats
		document.Products["file-team"] = team
		other := document.Products["other"]
		other.DisplayName = catalog.Value("Renamed product")
		document.Products["other"] = other
		receipt, err = a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.Empty(t, receipt.Conflicts)
		require.Empty(t, receipt.Warnings)
		require.Equal(t, 1, receipt.PricesChanged)
		require.Equal(t, 1, receipt.ProductsChanged)
		prices, err = a.alphaRemote.ListPrices(t.Context(), billing.PriceListParams{ProductKey: "file-team", Key: "seats", Archived: ptr(false)})
		require.NoError(t, err)
		require.Len(t, prices.Items, 1)
		current := prices.Items[0]
		require.NotEqual(t, original.ID, current.ID)
		require.EqualValues(t, 2, current.Version)
		require.Equal(t, &catalog.Quantity{Min: 1, Max: 50}, current.Quantity)
		archived, err = a.alphaRemote.GetPrice(t.Context(), original.ID)
		require.NoError(t, err)
		require.True(t, archived.Archived)
		require.Equal(t, original.Quantity, archived.Quantity)
		products, err := a.alphaRemote.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"other"}})
		require.NoError(t, err)
		require.Len(t, products.Items, 1)
		require.Equal(t, "Renamed product", products.Items[0].DisplayName)
		require.ElementsMatch(t, []billing.CatalogChange{
			{Object: billing.CatalogObjectPrice, Key: "seats", ProductKey: ptr("file-team"), Fields: []string{"quantity"}, Revision: current.Revision},
			{Object: billing.CatalogObjectProduct, Key: "other", Fields: []string{"display_name"}, Revision: products.Items[0].Revision},
		}, receipt.Changes)
	})
	t.Run("OR-J05-S15/product-creates-retry-by-content-including-concurrently", func(t *testing.T) {
		a := newAuthorityWorld(t)
		const path = "/v1/catalog/products"
		input := billing.CreateProductParams{Key: "retry-prod", DisplayName: "Retry"}
		status, raw := a.request(t, http.MethodPost, path, a.alphaKey.Secret, input)
		require.Equal(t, http.StatusCreated, status, string(raw))
		var first billing.Product
		require.NoError(t, json.Unmarshal(raw, &first))
		require.False(t, first.ID.IsZero())
		for _, headers := range []map[string]string{nil, {"Idempotency-Key": "product-retry"}, {"Idempotency-Key": "product-retry"}} {
			status, raw = a.request(t, http.MethodPost, path, a.alphaKey.Secret, input, headers)
			require.Equal(t, http.StatusCreated, status, string(raw))
			var replay billing.Product
			require.NoError(t, json.Unmarshal(raw, &replay))
			require.Equal(t, first, replay)
		}
		input.DisplayName = "Different"
		status, raw = a.request(t, http.MethodPost, path, a.alphaKey.Secret, input)
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"resource_conflict"`)

		// Match the stored create definition after trimming, sorting, currency
		// normalization, credit expiry defaults and the empty tier group's NULL.
		full := billing.CreateProductParams{
			Key: " retry-full ", DisplayName: " Retry full ", Description: " Description ",
			Entitlements: []string{"z:read", "a:read"}, RequiredEntitlements: []string{"z:owned", "a:owned"},
			CreditGrant: &catalog.CreditGrantSpec{Currency: "usd", Amount: ptr(int64(1_000_000))},
			TierGroup:   ptr(""), TierRank: 2, Ownership: catalog.OwnershipConsumable, Archived: true,
		}
		status, raw = a.request(t, http.MethodPost, path, a.alphaKey.Secret, full)
		require.Equal(t, http.StatusCreated, status, string(raw))
		var stored billing.Product
		require.NoError(t, json.Unmarshal(raw, &stored))
		full.Key, full.DisplayName, full.Description = "retry-full", "Retry full", "Description"
		full.Entitlements, full.RequiredEntitlements = []string{"a:read", "z:read"}, []string{"a:owned", "z:owned"}
		full.CreditGrant = &catalog.CreditGrantSpec{Currency: "USD", Amount: ptr(int64(1_000_000)), ExpiresAfterDays: ptr(365)}
		full.TierGroup = nil
		status, raw = a.request(t, http.MethodPost, path, a.alphaKey.Secret, full)
		require.Equal(t, http.StatusCreated, status, string(raw))
		var normalized billing.Product
		require.NoError(t, json.Unmarshal(raw, &normalized))
		require.Equal(t, stored, normalized)
		for _, change := range []func(*billing.CreateProductParams){
			func(p *billing.CreateProductParams) { p.DisplayName = "Different" },
			func(p *billing.CreateProductParams) { p.Description = "Different" },
			func(p *billing.CreateProductParams) { p.Entitlements = []string{"other:read"} },
			func(p *billing.CreateProductParams) { p.RequiredEntitlements = []string{"other:owned"} },
			func(p *billing.CreateProductParams) { p.CreditGrant = nil },
			func(p *billing.CreateProductParams) { p.TierGroup = ptr("other") },
			func(p *billing.CreateProductParams) { p.TierRank++ },
			func(p *billing.CreateProductParams) { p.Ownership = catalog.OwnershipUnique },
			func(p *billing.CreateProductParams) { p.Archived = false },
		} {
			conflict := full
			change(&conflict)
			status, raw = a.request(t, http.MethodPost, path, a.alphaKey.Secret, conflict)
			require.Equal(t, http.StatusConflict, status, string(raw))
			require.Contains(t, string(raw), `"code":"resource_conflict"`)
		}

		input = billing.CreateProductParams{Key: "retry-concurrent", DisplayName: "Concurrent retry"}
		type result struct {
			status int
			raw    []byte
			err    error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for range 2 {
			go func() {
				<-start
				s, b, err := a.do(t.Context(), http.MethodPost, path, a.alphaKey.Secret, input)
				results <- result{s, b, err}
			}()
		}
		close(start)
		observed := []result{<-results, <-results}
		var concurrent [2]billing.Product
		for i, r := range observed {
			require.NoError(t, r.err)
			require.Equal(t, http.StatusCreated, r.status, string(r.raw))
			require.NoError(t, json.Unmarshal(r.raw, &concurrent[i]))
			require.False(t, concurrent[i].ID.IsZero())
		}
		require.Equal(t, concurrent[0], concurrent[1])
	})
	t.Run("OR-J05-S14/meter-edits-preserve-document-keys-and-aggregations", func(t *testing.T) {
		a := newAuthorityWorld(t)
		document, err := catalog.ParseApplicationJSON([]byte(`{"schema_version":1,"meters":{"Mixed Case":{"event_type":"x","aggregation":"count"},"maximum":{"event_type":"x","value_property":"units","aggregation":"max"}}}`))
		require.NoError(t, err)
		receipt, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.Len(t, receipt.Changes, 2)
		require.Empty(t, receipt.Conflicts)

		path := "/v1/catalog/meters/" + url.PathEscape("Mixed Case")
		status, raw := a.request(t, http.MethodGet, path, a.alphaKey.Secret, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var meter billing.Meter
		require.NoError(t, json.Unmarshal(raw, &meter))
		require.Equal(t, "Mixed Case", meter.Key)
		require.True(t, meter.BillingSupported)
		originalRevision := meter.Revision
		params := billing.SetMeterParams{EventType: "edited", Aggregation: catalog.AggregationCount, ExpectedRevision: &originalRevision}
		status, raw = a.request(t, http.MethodPut, path, a.alphaKey.Secret, params)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &meter))
		require.Equal(t, "Mixed Case", meter.Key)
		require.Equal(t, "edited", meter.EventType)
		require.Greater(t, meter.Revision, originalRevision)
		read, err := a.alphaRemote.GetMeter(t.Context(), "Mixed Case")
		require.NoError(t, err)
		require.Equal(t, meter, *read)
		_, err = a.alphaRemote.GetMeter(t.Context(), "mixed-case")
		require.ErrorIs(t, err, billing.ErrNotFound, "editing must not create a normalized alias")

		// The optional card operations use the same exact meter key.
		product, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "meter-card", DisplayName: "Meter card"})
		require.NoError(t, err)
		params.ExpectedRevision = nil
		params.RateCard = catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 1, DivideBy: 1}}})
		read, err = a.alphaRemote.SetMeter(t.Context(), "Mixed Case", params)
		require.NoError(t, err)
		require.NotNil(t, read.RateCard)
		params.RateCard = catalog.Null[billing.MeterRateCardParams]()
		read, err = a.alphaRemote.SetMeter(t.Context(), "Mixed Case", params)
		require.NoError(t, err)
		require.Nil(t, read.RateCard)

		for _, key := range []string{strings.Repeat("m", 300), strings.Repeat("é", 128), " padded "} {
			_, err := catalog.ParseApplicationJSON([]byte(fmt.Sprintf(`{"schema_version":1,"meters":{%q:{"event_type":"x","aggregation":"count"}}}`, key)))
			require.Error(t, err)
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				status, raw := a.request(t, method, "/v1/catalog/meters/"+url.PathEscape(key), a.alphaKey.Secret, billing.SetMeterParams{EventType: "x", Aggregation: catalog.AggregationCount})
				require.Equal(t, http.StatusBadRequest, status, string(raw))
				var failure struct {
					Error billing.ErrorDetails `json:"error"`
				}
				require.NoError(t, json.Unmarshal(raw, &failure))
				require.Equal(t, billing.CodeInvalidParam, failure.Error.Code)
				require.Equal(t, ptr("key"), failure.Error.Param)
			}
		}
		read, err = a.alphaRemote.SetMeter(t.Context(), "has space", billing.SetMeterParams{EventType: "x", Aggregation: catalog.AggregationCount})
		require.NoError(t, err)
		require.Equal(t, "has space", read.Key)

		read, err = a.alphaRemote.GetMeter(t.Context(), "maximum")
		require.NoError(t, err)
		require.False(t, read.BillingSupported)
		read, err = a.alphaRemote.SetMeter(t.Context(), "maximum", billing.SetMeterParams{EventType: "edited", ValueProperty: "units", Aggregation: catalog.AggregationMax})
		require.NoError(t, err)
		require.Equal(t, catalog.AggregationMax, read.Aggregation)
		require.False(t, read.BillingSupported)
		read, err = a.alphaRemote.GetMeter(t.Context(), "maximum")
		require.NoError(t, err)
		require.Equal(t, "edited", read.EventType)
		require.False(t, read.BillingSupported)
		meters, err := a.alphaRemote.ListMeters(t.Context(), billing.PageRequest{})
		require.NoError(t, err)
		require.Len(t, meters.Items, 3, "invalid keys and aliases must leave no extra meters")
	})
	t.Run("OR-J05-S01/exact-price-quantity-and-provider-settlement", func(t *testing.T) {
		a, provider := newPaymentWorld(t)
		product, price := publicSale(t, a, "exact-usd", 9_990_000, nil)
		read, err := a.alphaRemote.GetPrice(t.Context(), price.ID)
		require.NoError(t, err)
		require.Equal(t, product.ID, read.ProductID)
		require.EqualValues(t, 9_990_000, read.UnitAmount)
		require.Equal(t, catalog.Currency("USD"), read.Currency)
		preview, err := a.alphaRemote.PreviewOrder(t.Context(), billing.PreviewOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.OrderLineParams{{PriceID: price.ID, Quantity: ptr(3)}}})
		require.NoError(t, err)
		require.EqualValues(t, 29_970_000, preview.Total)
		require.Len(t, preview.Lines, 1)
		require.Nil(t, preview.Lines[0].Refusal)
		require.EqualValues(t, 29_970_000, preview.Lines[0].Amount)
		order := publicPurchase(t, a, provider, price.ID, 3, 29_970_000, "exact-three")
		require.Equal(t, price.ID, *order.Lines[0].PriceID)
		effects := provider.effects(t)
		require.Len(t, effects, 1)
		require.Equal(t, "usd", effects[0].Currency)
		require.EqualValues(t, 2997, effects[0].Amount, "USD native units are micros; the external charge settles exact cents")
	})
	t.Run("OR-J05-S02/invalid-money-quantity-and-trial-cannot-create-a-price", func(t *testing.T) {
		t.Run("public-value-boundaries", func(t *testing.T) {
			for _, sample := range []struct {
				input    string
				units    int64
				currency string
			}{{"9.99 USD", 9_990_000, "USD"}, {"0.000001 USD", 1, "USD"}, {"9223372036854.775807 USD", math.MaxInt64, "USD"}, {"0.000000001 SOL", 1, "SOL"}} {
				doc, err := catalog.ParseApplicationJSON([]byte(fmt.Sprintf(`{"schema_version":1,"products":{"exact":{"display_name":"Exact","prices":{"exact":{"amount":%q}}}}}`, sample.input)))
				require.NoError(t, err)
				price := doc.Products["exact"].Prices["exact"]
				require.Equal(t, sample.units, price.UnitAmount.Value)
				require.Equal(t, catalog.Currency(sample.currency), price.Currency.Value)
			}
			for _, sample := range []struct {
				input string
				hours int
			}{{"1 hour", 1}, {"30 days", 720}, {"2 weeks", 336}, {"2562047 hours", 2562047}} {
				hours, err := syntax.ParseDurationHours(sample.input)
				require.NoError(t, err)
				require.Equal(t, sample.hours, hours)
			}
			for _, invalid := range []string{"0 hours", "-1 day", "1.5 days", "1 month", "2562048 hours", "9223372036854775807 weeks"} {
				_, err := syntax.ParseDurationHours(invalid)
				require.Error(t, err, invalid)
			}
			id, err := billing.NewPriceID("media/a:b", "monthly plus", 7)
			require.NoError(t, err)
			require.Equal(t, "media%2Fa%3Ab:monthly%20plus:v7", id.String())
			parsed, err := billing.ParsePriceID("media%2Fa%3Ab:monthly%20plus:v7")
			require.NoError(t, err)
			require.Equal(t, id, parsed)
			for _, invalid := range []string{"media%2fa%3Ab:monthly%20plus:v7", "media/a:monthly:v1", "media:monthly:v0", "media:monthly:v01", "media:monthly:v9223372036854775808", "media%00:monthly:v1"} {
				_, err := billing.ParsePriceID(invalid)
				require.Error(t, err, invalid)
			}
			_, err = billing.ParseProductID("pay_00000000-0000-4000-8000-000000000001")
			require.Error(t, err, "resource kind cannot be exchanged through its wire ID")
			_, err = billing.ParseMerchantID("")
			require.Error(t, err, "a merchant identity is required")
			for _, sample := range []struct {
				wire   string
				amount int64
			}{{"9007199254740993", 9_007_199_254_740_993}, {"9223372036854775807", math.MaxInt64}, {"-9223372036854775808", math.MinInt64}} {
				var payment billing.Payment
				require.NoError(t, json.Unmarshal([]byte(`{"amount":"`+sample.wire+`","currency":"USD"}`), &payment))
				require.Equal(t, sample.amount, payment.Amount)
				raw, err := json.Marshal(payment)
				require.NoError(t, err)
				var encoded map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(raw, &encoded))
				require.Equal(t, `"`+sample.wire+`"`, string(encoded["amount"]))
			}
			for _, invalid := range []string{`{"amount":"9223372036854775808"}`, `{"amount":"-9223372036854775809"}`, `{"amount":9.99}`, `{"amount":"1e6"}`} {
				var payment billing.Payment
				require.Error(t, json.Unmarshal([]byte(invalid), &payment), invalid)
			}
		})
		for _, amount := range []string{"9.9900001 USD", "9223372036854.775808 USD", "9.99 UNKNOWN", "-1 USD"} {
			t.Run(amount, func(t *testing.T) {
				_, err := catalog.ParseApplicationJSON([]byte(fmt.Sprintf(`{"schema_version":1,"products":{"bad":{"display_name":"Bad","prices":{"bad":{"amount":%q}}}}}`, amount)))
				require.Error(t, err, "a public catalog document must refuse rounding, overflow, unknown currencies and negative prices")
			})
		}
		a, provider := newPaymentWorld(t)
		product, price := publicSale(t, a, "input-boundaries", 9_990_000, nil)
		for _, invalid := range []billing.CreatePriceParams{
			{ProductID: product.ID, Key: "wrong-currency", UnitAmount: 1, Currency: "UNKNOWN"},
			{ProductID: product.ID, Key: "negative", UnitAmount: -1, Currency: "USD"},
			{ProductID: product.ID, Key: "unsupported-trial", UnitAmount: 1_000_000, Currency: "USD", TrialUnitAmount: ptr(int64(0)), TrialDurationHours: ptr(24)},
		} {
			_, err := a.alphaRemote.CreatePrice(t.Context(), invalid)
			require.ErrorIs(t, err, billing.ErrInvalid)
		}
		for _, quantity := range []int{-1, 0} {
			_, err := a.alphaRemote.CreateOrder(t.Context(), fmt.Sprint("invalid-quantity-", quantity), billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.OrderLineParams{{PriceID: price.ID, Quantity: ptr(quantity)}}})
			require.Error(t, err)
		}
		prices, err := a.alphaRemote.ListPrices(t.Context(), billing.PriceListParams{ProductID: product.ID})
		require.NoError(t, err)
		require.Len(t, prices.Items, 1)
		require.Equal(t, price.ID, prices.Items[0].ID)
		orders, err := a.alphaRemote.ListOrders(t.Context(), billing.OrderListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Empty(t, orders.Items)
		require.Empty(t, provider.effects(t))
	})
	t.Run("OR-J05-S03/replay-manual-ownership-and-revision-conflicts", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		document, err := catalog.ParseApplicationJSON([]byte(`{"schema_version":1,"products":{"declared":{"display_name":"Declared name","entitlements":["declared:read"],"prices":{"standard":{"amount":"9.99 USD"}}}}}`))
		require.NoError(t, err)
		first, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.Equal(t, 1, first.ProductsChanged)
		require.Equal(t, 1, first.PricesChanged)
		replay, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.Equal(t, first.ApplicationID, replay.ApplicationID)
		require.Equal(t, first.AppliedRevision, replay.AppliedRevision)
		products, err := a.alphaRemote.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"declared"}})
		require.NoError(t, err)
		require.Len(t, products.Items, 1)
		product := products.Items[0]
		manual, err := a.alphaRemote.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Manual name"), ExpectedRevision: &product.Revision})
		require.NoError(t, err)
		_, err = a.alphaRemote.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("Stale name"), ExpectedRevision: &product.Revision})
		var failure *billing.StatusError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, billing.CodeRevisionMismatch, failure.Code)
		declaration := document.Products["declared"]
		declaration.DisplayName = catalog.Value("Reconciled name")
		document.Products["declared"] = declaration
		conflict, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.Len(t, conflict.Conflicts, 1)
		require.Equal(t, "declared", conflict.Conflicts[0].Key)
		kept, err := a.alphaRemote.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.Equal(t, "Manual name", kept.DisplayName)
		require.Equal(t, manual.Revision, kept.Revision)
		forced, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{Force: true})
		require.NoError(t, err)
		require.Empty(t, forced.Conflicts)
		final, err := a.alphaRemote.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.Equal(t, "Reconciled name", final.DisplayName)
		require.Greater(t, final.Revision, manual.Revision)
	})
	t.Run("OR-J05-S04/archive-keeps-purchased-access-and-money-history", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		product, price := publicSale(t, a, "archive", 9_990_000, nil)
		order := publicPurchase(t, a, p, price.ID, 1, 9_990_000, "before-archive")
		archived, err := a.alphaRemote.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
		require.NoError(t, err)
		require.True(t, archived.Archived)
		kept, err := a.alphaRemote.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderComplete, kept.Status)
		require.EqualValues(t, 9_990_000, kept.Total)
		require.Equal(t, product.ID, *kept.Lines[0].ProductID)
		payment, err := a.alphaRemote.GetPayment(t.Context(), *order.Payment.PaymentID)
		require.NoError(t, err)
		require.EqualValues(t, 9_990_000, payment.Amount)
		access, err := a.alphaRemote.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: order.CustomerID, Entitlements: []string{"library:read"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["library:read"])
		_, err = a.alphaRemote.CreateOrder(t.Context(), "after-archive", billing.CreateOrderParams{CustomerID: order.CustomerID, Lines: []billing.OrderLineParams{{PriceID: price.ID}}})
		require.Error(t, err)
		require.Len(t, p.effects(t), 1)
	})
	t.Run("OR-J05-S05/accepted-order-terms-survive-price-and-PSP-edits", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		product, price := publicSale(t, a, "immutable", 9_990_000, ptr(24))
		order, err := a.alphaRemote.CreateOrder(t.Context(), "accepted-terms", billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Lines: []billing.OrderLineParams{{PriceID: price.ID, Quantity: ptr(3)}}})
		require.NoError(t, err)
		next, err := a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: price.Key, UnitAmount: 14_990_000, Currency: "USD", AccessDurationHours: ptr(48), ExpectedRevision: &price.Revision})
		require.NoError(t, err)
		require.NotEqual(t, price.ID, next.ID)
		psps, err := a.alphaRemote.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		// This host owns a declared-file profile. PSP mutation through the
		// merchant API requires Vault; the supported edit here is a real
		// restart with the revised declaration, preserving the same database.
		cfg := a.cfg
		cfg.Merchants = slices.Clone(a.cfg.Merchants)
		declaration := cfgMerchant(cfg, "alpha")
		declaration.PSPs = billing.PSPs{"stripe": openrails.StripePSP{AccountID: "acct_e2e", Environment: billing.EnvironmentTest, SecretKey: "sk_test_critical", WebhookSigningSecret: "whsec_critical_fixture", PublishableKey: "pk_test_rotated"}.PSPConfig()}
		setCfgMerchant(&cfg, "alpha", declaration)
		a.server.Close()
		require.NoError(t, a.engine.Close(t.Context()))
		reopened, err := openrails.New(t.Context(), cfg, a.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reopened.Close(context.Background())) })
		mux := http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(mux, reopened, openrails.Routes{Auth: a.auth.Authenticator(), RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Programmatic: true}}))
		a.server = httptest.NewServer(mux)
		t.Cleanup(a.server.Close)
		a.alphaRemote = a.remote(t, a.alpha.MerchantID(), a.alphaKey.Secret)
		updatedPSP, err := a.alphaRemote.GetPSP(t.Context(), psps.Items[0].ID)
		require.NoError(t, err)
		require.Equal(t, "pk_test_rotated", updatedPSP.PublishableKey)
		kept, err := a.alphaRemote.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.EqualValues(t, 29_970_000, kept.Total)
		require.Equal(t, price.ID, *kept.Lines[0].PriceID)
		require.EqualValues(t, 9_990_000, kept.Lines[0].UnitAmount)
		require.Equal(t, 24, *kept.Lines[0].AccessDurationHours)
		status, raw := a.request(t, "POST", "/v1/me/orders/"+order.ID.String()+"/pay", a.aliceToken, billing.PayOrderParams{ExpectedTotal: 29_970_000, Payment: billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}, map[string]string{"Idempotency-Key": "immutable-order-pay"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var paid billing.Order
		require.NoError(t, json.Unmarshal(raw, &paid))
		require.Equal(t, order.ID, paid.ID)
		require.Equal(t, billing.OrderComplete, paid.Status)
		require.EqualValues(t, 29_970_000, paid.Total)
		effects := p.effects(t)
		require.Len(t, effects, 1)
		require.EqualValues(t, 2997, effects[0].Amount)
	})
	t.Run("OR-J05-S13/prune-keeps-a-refused-price-and-its-ownership", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		document, err := catalog.ParseApplicationJSON([]byte(`{"schema_version":1,"products":{"pro":{"display_name":"Pro","prices":{"monthly":{"amount":"10.00 USD","billing_interval":"30 days"},"omitted":{"amount":"2.00 USD"}}}}}`))
		require.NoError(t, err)
		first, err := a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.Empty(t, first.Warnings)
		prices, err := a.alphaRemote.ListPrices(t.Context(), billing.PriceListParams{ProductKey: "pro", Key: "monthly"})
		require.NoError(t, err)
		require.Len(t, prices.Items, 1)
		price := prices.Items[0]
		require.EqualValues(t, 1, price.Version)
		require.False(t, price.Archived)
		omitted, err := a.alphaRemote.ListPrices(t.Context(), billing.PriceListParams{ProductKey: "pro", Key: "omitted"})
		require.NoError(t, err)
		require.Len(t, omitted.Items, 1)
		ownersQuery := "SELECT jsonb_agg(o ORDER BY field, manager) FROM " + pgx.Identifier{a.schema, "catalog_field_owners"}.Sanitize() + " o WHERE merchant_id = $1 AND object = 'price' AND key = 'pro' AND price_key = 'monthly'"
		var ownersBefore, ownersAfter []byte
		require.NoError(t, a.pool.QueryRow(t.Context(), ownersQuery, a.alpha.MerchantID().UUID()).Scan(&ownersBefore))
		require.NotEmpty(t, ownersBefore)

		prune, err := catalog.ParseApplicationJSON([]byte(`{"schema_version":1,"prune":true,"products":{"pro":{"display_name":"Pro","prices":{"monthly":{"amount":"10.00 USD","billing_interval":"30 days","sell_via":["solana"]}}}}}`))
		require.NoError(t, err)
		receipt, err := a.alphaRemote.ApplyCatalog(t.Context(), prune, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		require.Equal(t, []billing.CatalogChange{{Object: billing.CatalogObjectPrice, Key: "omitted", ProductKey: ptr("pro"), Fields: []string{}, Revision: omitted.Items[0].Revision, Deleted: true}}, receipt.Changes)
		require.Equal(t, 0, receipt.ProductsChanged)
		require.Equal(t, 1, receipt.PricesChanged)
		require.Empty(t, receipt.Conflicts)
		require.Len(t, receipt.Warnings, 1)
		require.True(t, receipt.Warnings[0].Refused)
		require.Equal(t, "pro", receipt.Warnings[0].ProductKey)
		require.Equal(t, "monthly", receipt.Warnings[0].Key)
		require.Contains(t, receipt.Warnings[0].Message, "Solana")
		kept, err := a.alphaRemote.GetPrice(t.Context(), price.ID)
		require.NoError(t, err)
		require.Equal(t, price, *kept)
		live, err := a.alphaRemote.ListPrices(t.Context(), billing.PriceListParams{ProductKey: "pro", Archived: ptr(false)})
		require.NoError(t, err)
		require.Equal(t, []billing.Price{price}, live.Items)
		require.NoError(t, a.pool.QueryRow(t.Context(), ownersQuery, a.alpha.MerchantID().UUID()).Scan(&ownersAfter))
		require.Equal(t, ownersBefore, ownersAfter)
		_, err = a.alphaRemote.GetPrice(t.Context(), omitted.Items[0].ID)
		require.ErrorIs(t, err, billing.ErrNotFound)
		var recorded bool
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM "+pgx.Identifier{a.schema, "catalog_applications"}.Sanitize()+" WHERE merchant_id = $1 AND application_id = $2)", a.alpha.MerchantID().UUID(), receipt.ApplicationID).Scan(&recorded))
		require.False(t, recorded)
	})
}
