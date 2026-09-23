//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails"

	"github.com/open-rails/openrails/internal/testauth"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/catalog"
)

func intPtr(v int) *int { return &v }

func TestExampleCatalogAppliesOverHTTP(t *testing.T) {
	h, f := newCatalogWorkflow(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.example.yaml"))
	require.NoError(t, err)
	application, err := catalog.ParseApplicationYAML(raw)
	require.NoError(t, err)
	revision, err := f.client.Catalog.Revision(t.Context())
	require.NoError(t, err)
	application.ApplicationID = uuid.NewString()
	application.ExpectedRevision = &revision.Revision
	first, err := f.client.Catalog.Apply(t.Context(), application)
	require.NoError(t, err)
	again, err := f.client.Catalog.Apply(t.Context(), application)
	require.NoError(t, err)
	require.True(t, again.Replayed)
	require.Equal(t, first.AppliedRevision, again.AppliedRevision)
	var products, prices int
	require.NoError(t, h.Pool().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM billing.products WHERE merchant_id=$1),(SELECT count(*) FROM billing.prices WHERE merchant_id=$1)`, f.merchant.MerchantID.UUID()).Scan(&products, &prices))
	require.Equal(t, len(application.Products), products)
	wantPrices := 0
	for _, product := range application.Products {
		wantPrices += len(product.Prices)
	}
	require.Equal(t, wantPrices, prices)
}

// TestCatalogApplicationRateCardsHTTP drives the full manifest -> apply -> DB path for
// the #638/#639 rate-card model: a usage product priced by a matrix rate card
// (and no flat prices) and a variable credit-purchase product, published over
// HTTP, must create the products AND persist their rate-card / credit-purchase
// sidecars. Guards the applier mapping that the spec-level sidecar test skips.
func TestCatalogApplicationRateCardsHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	token := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-ratecards-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	meterKey := "droplet-seconds-" + suffix
	dropletKey := "droplet-" + suffix
	mid := dbtest.TestMerchantID.UUID()
	t.Cleanup(func() {
		_, _ = h.Pool().Exec(ctx, "DELETE FROM billing.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", mid, meterKey)
		_, _ = h.Pool().Exec(ctx, "DELETE FROM billing.catalog_meters WHERE merchant_id = $1 AND key = $2", mid, meterKey)
		_, _ = h.Pool().Exec(ctx, "DELETE FROM billing.products WHERE merchant_id = $1 AND key = ANY($2::text[])", mid, []string{dropletKey})
	})

	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		Meters: []catalog.Meter{{
			Key:           meterKey,
			EventType:     "droplet.usage",
			ValueProperty: "$.seconds",
			Aggregation:   "sum",
			GroupBy:       map[string]string{"size_slug": "$.size_slug"},
		}},
		Products: []catalog.Product{
			{
				Key:         dropletKey,
				DisplayName: "Droplet", // usage-metered: no tier_group, no billing cadence (#642)
				RateCards: []catalog.RateCard{{
					Meter: meterKey,
					Price: catalog.RatePrice{
						Model: "per_unit", Currency: "USD",
						PerUnit: &catalog.PerUnitPrice{
							DivideBy: 3600,
							Matrix: &catalog.Matrix{Dimension: "size_slug", Cells: map[string]catalog.MatrixCell{
								"s-1vcpu-1gb": {UnitAmount: 8930, MaximumAmount: 6_000_000},
							}},
						},
					},
				}},
			},
		},
	}

	applyStatus, applyBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/applications", token, catalogApplicationFixture(t, surface.BaseURL, token, manifest))
	require.Equal(t, http.StatusOK, applyStatus, string(applyBody))

	// The matrix rate card persisted and links its meter + product.
	var model, rcMeter string
	require.NoError(t, h.Pool().QueryRow(ctx, `
SELECT rc.price ->> 'model', rc.meter_key
FROM billing.catalog_rate_cards rc
JOIN billing.products p ON p.id = rc.product_id
WHERE p.merchant_id = $1 AND p.key = $2`, mid, dropletKey).Scan(&model, &rcMeter))
	require.Equal(t, "per_unit", model)
	require.Equal(t, meterKey, rcMeter)

	// The rate-card meter persisted with its aggregation.
	var agg string
	require.NoError(t, h.Pool().QueryRow(ctx, `
SELECT aggregation FROM billing.catalog_meters WHERE merchant_id = $1 AND key = $2`, mid, meterKey).Scan(&agg))
	require.Equal(t, "sum", agg)
}

// or#896: a `trial:` first phase declared on a rail that cannot execute one
// (NMI, Solana) is REFUSED at publish, naming the limitation — it used to be
// accepted, dropped, and the subscriber charged the full amount immediately.
// The rails that can execute one (Stripe, CCBill) still publish.
type trialCapabilityNoNetwork struct{}

func (trialCapabilityNoNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("trial capability fixture forbids outbound Stripe calls to %s", r.URL.Host)
}

func TestCatalogApplicationRefusesTrialOnRailsWithoutFirstPhase(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd", WithConfig(func(cfg *config.Config) {}))
	// This checks local capability rules with unarmed declarations. Any provider
	// request is a fixture bug; refuse it under the real Stripe choke point.
	surface.App().Runtime.StripeClients = stripeapi.NewFactory(trialCapabilityNoNetwork{})
	owned := surface.ProvisionOwnedMerchant("trial-capability-" + uuid.NewString()[:8])
	token := surface.MintAPIKey(
		owned.MerchantSlug,
		"catalog-trials-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)
	mid := owned.MerchantID.UUID()
	for _, rail := range []string{"nmi", "solana", "stripe", "ccbill"} {
		dbtest.EnsureTestPSP(ctx, t, h.MerchantPool(mid), mid, rail)
	}

	publish := func(t *testing.T, psp string) (int, []byte, string) {
		t.Helper()
		productKey := "trial-guard-" + strings.ReplaceAll(uuid.NewString(), "-", "")
		t.Cleanup(func() {
			_, _ = h.Pool().Exec(ctx, "DELETE FROM billing.prices WHERE merchant_id = $1 AND product_id IN (SELECT id FROM billing.products WHERE merchant_id = $1 AND key = $2)", mid, productKey)
			_, _ = h.Pool().Exec(ctx, "DELETE FROM billing.products WHERE merchant_id = $1 AND key = $2", mid, productKey)
		})
		manifest := catalog.Manifest{
			Version: catalog.SupportedVersion,
			Products: []catalog.Product{{
				Key:         productKey,
				DisplayName: "Trial Guard",
				TierGroup:   "trial-guard-" + psp,
				Prices: []catalog.Price{{
					Currency:   "USD",
					UnitAmount: 23_000_000,
					Duration:   "30d",
					AutoRenew:  true,
					PSPs:       []string{psp},
					Trial:      &catalog.PriceTrial{UnitAmount: 0, Duration: "7d"},
				}},
			}},
		}
		if psp == "ccbill" {
			manifest.Products[0].Prices[0].PSPLinks = map[string]map[string]string{"ccbill": {"form_name": "trial-form", "flex_id": "trial-flex"}}
		}
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/applications", token, catalogApplicationFixture(t, surface.BaseURL, token, manifest))
		return status, body, productKey
	}

	for _, psp := range []string{"nmi", "solana"} {
		t.Run(psp+" is refused", func(t *testing.T) {
			status, body, productKey := publish(t, psp)
			require.Equal(t, http.StatusBadRequest, status, string(body))
			requireAPIErrorCode(t, body, "trial_unsupported_on_rail")
			require.Contains(t, string(body), "trial")

			// The refusal is total: no price row was written for the product.
			var priceCount int
			require.NoError(t, h.Pool().QueryRow(ctx, `
SELECT count(*) FROM billing.prices pr
JOIN billing.products p ON p.id = pr.product_id
WHERE p.merchant_id = $1 AND p.key = $2`, mid, productKey).Scan(&priceCount))
			require.Zero(t, priceCount, "a refused trial must leave no price behind")
		})
	}

	for _, psp := range []string{"stripe", "ccbill"} {
		t.Run(psp+" still publishes", func(t *testing.T) {
			status, body, productKey := publish(t, psp)
			require.Equal(t, http.StatusOK, status, string(body))

			var trialAmount *int64
			var trialHours *int
			require.NoError(t, h.Pool().QueryRow(ctx, `
SELECT pr.trial_unit_amount, pr.trial_duration_hours FROM billing.prices pr
JOIN billing.products p ON p.id = pr.product_id
WHERE p.merchant_id = $1 AND p.key = $2`, mid, productKey).Scan(&trialAmount, &trialHours))
			require.NotNil(t, trialAmount)
			require.Equal(t, int64(0), *trialAmount)
			require.NotNil(t, trialHours)
			require.Equal(t, 7*24, *trialHours)
		})
	}
}

func ptrI64(v int64) *int64 { return &v }

func TestNativeCatalogLifecycleHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	standalone := h.StartStandalone("usd")
	embedded := h.StartEmbeddedHost("usd")

	token := standalone.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-native-lifecycle-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)

	productKey := "native-lifecycle-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		Products: []catalog.Product{{
			Key:          productKey,
			DisplayName:  "Native Lifecycle Product",
			Description:  "published catalog anchor for native lifecycle proof",
			Entitlements: []string{"native-lifecycle-premium"},
			Prices: []catalog.Price{{
				UnitAmount: 10_000,
				Currency:   "USD",
				Duration:   "indefinite",
				PSPs:       []string{},
			}},
		}},
	}
	require.NoError(t, manifest.Validate())
	status, body := requestJSON(t, http.MethodPost, standalone.BaseURL+"/v1/merchant/catalog/applications", token, catalogApplicationFixture(t, standalone.BaseURL, token, manifest))
	require.Equal(t, http.StatusOK, status, string(body))

	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-key/"+productKey, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var product billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(body, &product))
	require.NotEqual(t, uuid.Nil, product.ID)

	for _, surface := range []*Surface{standalone, embedded} {
		t.Run(surface.Name, func(t *testing.T) {
			proveNativeCatalogLifecycle(t, h, surface, product.ID.UUID())
		})
	}
}

// TestNativeCatalogRateCardUsageHTTP proves the or#893 pricing input: a rate
// card published over HTTP is the ONE way usage is priced (no price row, no
// metered: sugar, no catalog_price_metered), and reported usage is rated
// through it onto a finalized invoice.
//
// It also pins the removal of the #599 counter bridge: the event that carries
// no quantity contributes 0, not 1. "Count the event itself" is now declared,
// not inferred — it is aggregation: count.
func TestNativeCatalogRateCardUsageHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	standalone := h.StartStandalone("usd")

	token := standalone.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-metered-usage-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)
	productKey := "metered-usage-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	meterKey := "vm-seconds-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		Meters: []catalog.Meter{{
			Key:           meterKey,
			ValueProperty: meterKey,
			Aggregation:   "sum",
		}},
		Products: []catalog.Product{{
			Key:         productKey,
			DisplayName: "Metered Usage Product",
			RateCards: []catalog.RateCard{{
				Meter:       meterKey,
				PaymentTerm: catalog.PaymentInArrears,
				Price: catalog.RatePrice{
					Model:    "per_unit",
					Currency: "USD",
					PerUnit:  &catalog.PerUnitPrice{UnitAmount: 250_000, DivideBy: 100},
				},
			}},
		}},
	}
	require.NoError(t, manifest.Validate())
	status, body := requestJSON(t, http.MethodPost, standalone.BaseURL+"/v1/merchant/catalog/applications", token, catalogApplicationFixture(t, standalone.BaseURL, token, manifest))
	require.Equal(t, http.StatusOK, status, string(body))

	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-key/"+productKey, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var product billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(body, &product))

	// Pure usage: no price row, one rate card carrying unit_amount/divide_by,
	// one meter.
	prices, err := (httpCatalogApplier{t: t, baseURL: standalone.BaseURL, token: token}).ListPricesByProduct(ctx, product.ID, true)
	require.NoError(t, err)
	require.Empty(t, prices)
	var meterCount int
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.catalog_meters WHERE merchant_id = $1 AND key = $2`, dbtest.TestMerchantID.UUID(), meterKey).Scan(&meterCount))
	require.Equal(t, 1, meterCount)
	var unitAmount, divideBy int64
	require.NoError(t, h.Pool().QueryRow(ctx, `
SELECT (price -> 'per_unit' ->> 'unit_amount')::bigint, (price -> 'per_unit' ->> 'divide_by')::bigint
FROM billing.catalog_rate_cards
WHERE merchant_id = $1 AND product_id = $2 AND meter_key = $3 AND payment_term = 'in_arrears'`,
		dbtest.TestMerchantID.UUID(), product.ID, meterKey).Scan(&unitAmount, &divideBy))
	require.Equal(t, int64(250_000), unitAmount)
	require.Equal(t, int64(100), divideBy)

	// Rate reported usage through the card: three quantity-bearing events
	// (100+200+120) plus one without the dimension, which contributes 0 now that
	// the counter bridge is gone — aggregate 420 ->
	// round_half_up(420 * 250_000 / 100) = 1_050_000.
	mctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	payerID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(mctx, "DELETE FROM billing.usage_events WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(mctx, "DELETE FROM billing.invoice_items WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(mctx, "DELETE FROM billing.invoices WHERE customer_id = $1", payerID)
	})
	moneySvc := money.NewMoneyService(dbi)
	payer := identity.CustomerID(payerID)
	for _, quantity := range []int64{100, 200, 120} {
		_, err := moneySvc.RecordUsage(mctx, money.RecordUsageParams{
			Payer:      &payer,
			Invoker:    "test-invoker",
			Currency:   "USD",
			EventType:  meterKey,
			Dimensions: map[string]int64{meterKey: quantity},
			Amount:     0,
			Key:        money.MustIdempotencyKey(money.UsageOperation(meterKey), "metered-usage-http", uuid.NewString()),
			OccurredAt: time.Now(),
		})
		require.NoError(t, err)
	}
	_, err = moneySvc.RecordUsage(mctx, money.RecordUsageParams{
		Payer:      &payer,
		Invoker:    "test-invoker",
		Currency:   "USD",
		EventType:  meterKey,
		Amount:     0,
		Key:        money.MustIdempotencyKey(money.UsageOperation(meterKey), "metered-usage-http", uuid.NewString()),
		OccurredAt: time.Now(),
	})
	require.NoError(t, err)

	inv, err := moneySvc.FinalizeInvoice(mctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, int64(1_050_000), inv.AmountDue)
}

func liveOwnershipGrantCount(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, customer, product uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM billing.grants g
WHERE g.merchant_id = $1
  AND g.customer_id = $2
  AND g.product_id = $3
  AND g.kind = 'ownership'
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants t
      WHERE t.merchant_id = g.merchant_id
        AND t.supersedes_id = g.id
        AND t.event IN ('revoke', 'expire', 'supersede')
  )`, dbtest.TestMerchantID.UUID(), customer, product).Scan(&n))
	return n
}

func mustCatalogProduct(t *testing.T, ctx context.Context, applier *openrails.Client, key string) openrails.Product {
	t.Helper()
	product, err := applier.Products.RetrieveByKey(ctx, key)
	require.NoError(t, err)
	return *product
}

func proveNativeCatalogLifecycle(t *testing.T, h *Harness, surface *Surface, productID uuid.UUID) {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	payer := openrails.CustomerID(uuid.New())
	payerID := payer.UUID()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, payerID.String())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoice_items WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.usage_events WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoices WHERE customer_id = $1", payerID)
	})

	grantLedger := grants.New(dbtest.Queries(pool), dbtest.TestMerchantID.UUID())
	entitlementGrant, err := grantLedger.Grant(ctx, grants.GrantInput{
		Customer: payerID,
		Product:  &productID,
		Kind:     grants.Entitlement,
		Source:   grants.Purchase,
		SourceID: uuid.NewString(),
		Spec:     &grants.Spec{Entitlements: []string{"native-lifecycle-premium"}},
	})
	require.NoError(t, err)
	require.NoError(t, grantLedger.MaterializeGrant(ctx, entitlementGrant))

	status, body := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/customers/"+payerID.String()+"/entitlements", surface.Token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var entitlementRows []struct {
		Entitlement string `json:"entitlement"`
	}
	require.NoError(t, json.Unmarshal(body, &entitlementRows))
	require.Len(t, entitlementRows, 1)
	require.Equal(t, "native-lifecycle-premium", entitlementRows[0].Entitlement)

	client := surface.Client()
	depositSourceID := uuid.NewString()
	_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{
		CustomerID: new(payer.String()),
		Invoker:    payerID.String(),
		Currency:   "USD",
		Amount:     10_000,
		Source:     "catalog-native-lifecycle",
		SourceID:   depositSourceID,
	})
	require.NoError(t, err)
	balance, err := client.Balance(ctx, (openrails.CustomerID(payerID)).String())
	require.NoError(t, err)
	require.Equal(t, int64(10_000), balance.BalanceAmount)

	requestID := "native-lifecycle-" + surface.Name + "-" + uuid.NewString()
	verdicts, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{{
		CustomerID:      (openrails.CustomerID(payerID)).String(),
		Invoker:         payerID.String(),
		InvokerType:     string(identity.InvokerTypePayer),
		Resource:        "vm-small",
		Currency:        "USD",
		EstimatedAmount: 2_500,
		ExpiresAt:       holdDeadline(),
		RequestID:       requestID,
		Source:          "native-lifecycle",
	}})
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])
	capture, err := client.Capture(ctx, requestID, 2_000, &openrails.CaptureUsage{
		EventType: "vm-runtime",
		Resource:  "vm-small",
		Metadata:  map[string]any{"tier": "basic"},
		Source:    "native-lifecycle",
		SourceID:  requestID,
	})
	require.NoError(t, err)
	require.EqualValues(t, 2_000, capture.Amount)

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)

	rows, err := client.UsageRollup(ctx, (openrails.CustomerID(payerID)).String(), "usd", from, to, "resource")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "vm-small", rows[0].Key)
	require.Equal(t, int64(2_000), rows[0].TotalAmount)

	inv, err := money.NewMoneyService(dbi).FinalizeInvoice(ctx, identity.CustomerID(payerID), money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, "paid", inv.Status)
	require.Equal(t, int64(2_000), inv.UsageTotal)
	require.Equal(t, int64(0), inv.AmountDue)

	invAgain, err := money.NewMoneyService(dbi).FinalizeInvoice(ctx, identity.CustomerID(payerID), money.DefaultCurrency, from, to)
	require.NoError(t, err)
	require.Equal(t, inv.ID, invAgain.ID)
}

func requestJSON(t *testing.T, method, url, token string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, &buf)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		require.NoError(t, testauth.Authorize(req, token))
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

type httpCatalogApplier struct {
	t       *testing.T
	baseURL string
	token   string
}

// The external Solana proof uses this one convenience method. Its requests
// now go through the shared Client, not another HTTP implementation.
func (a httpCatalogApplier) ListPricesByProduct(ctx context.Context, id openrails.ProductID, activeOnly bool) ([]openrails.Price, error) {
	client, err := openrails.NewRemote(a.baseURL, openrails.WithAPIKey(a.token), openrails.WithMerchantID(dbtest.TestMerchantID), openrails.WithCurrency("USD"))
	if err != nil {
		return nil, err
	}
	return catalogPrices(ctx, client, id, activeOnly)
}

// catalogApplicationFixture preserves the existing native catalog fixture data
// while sending only the new public application contract over HTTP.
func catalogApplicationFixture(t *testing.T, baseURL, token string, m catalog.Manifest) *catalog.Application {
	t.Helper()
	status, raw := requestJSON(t, http.MethodGet, baseURL+"/v1/merchant/catalog/revision", token, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var revision openrails.CatalogRevision
	require.NoError(t, json.Unmarshal(raw, &revision))
	result := &catalog.Application{SchemaVersion: 1, ApplicationID: uuid.NewString(), ExpectedRevision: &revision.Revision}
	for _, meter := range m.Meters {
		result.Meters = append(result.Meters, catalog.ApplyMeter{Key: meter.Key, EventType: catalog.Value(meter.EventType), ValueProperty: catalog.Value(meter.ValueProperty), Aggregation: catalog.Value(meter.Aggregation), Unit: catalog.Value(meter.Unit), GroupBy: catalog.Value(meter.GroupBy)})
	}
	for _, product := range m.Products {
		entitlements := map[string]*int{}
		for _, key := range product.Entitlements {
			entitlements[key] = nil
		}
		p := catalog.ApplyProduct{Key: product.Key, DisplayName: catalog.Value(product.DisplayName), Description: catalog.Value(product.Description), Archived: catalog.Value(product.Archived), EntitlementsSpec: catalog.Value(entitlements)}
		if product.TierGroup != "" {
			p.TierGroup = catalog.Value(product.TierGroup)
		}
		if product.TierRank != nil {
			p.TierRank = catalog.Value(*product.TierRank)
		}
		if product.RateCards != nil {
			p.RateCards = catalog.Value(product.RateCards)
		}
		for index, price := range product.Prices {
			key := price.Key
			if key == "" {
				key = fmt.Sprintf("%s-offer-%d", product.Key, index)
				if price.Duration == "30d" || price.Duration == "720h" {
					key = product.Key + "-monthly"
				}
			}
			out := catalog.ApplyPrice{Key: key, Currency: catalog.Value(price.Currency), UnitAmount: catalog.Value(price.UnitAmount), AutoRenew: catalog.Value(price.AutoRenew), Archived: catalog.Value(price.Archived)}
			if price.Duration != "" && price.Duration != "indefinite" {
				duration, err := catalog.ParseDurationSpec(price.Duration)
				require.NoError(t, err)
				out.AccessDurationHours = catalog.Value(int(duration / time.Hour))
			}
			if price.Trial != nil {
				duration, err := catalog.ParseDurationSpec(price.Trial.Duration)
				require.NoError(t, err)
				out.TrialDurationHours = catalog.Value(int(duration / time.Hour))
				out.TrialUnitAmount = catalog.Value(price.Trial.UnitAmount)
			}
			if len(price.PSPs) > 0 {
				out.PSPs = catalog.Value(price.PSPs)
			}
			if len(price.PSPLinks) > 0 {
				out.PSPLinks = catalog.Value(price.PSPLinks)
			}
			p.Prices = append(p.Prices, out)
		}
		result.Products = append(result.Products, p)
	}
	return result
}

// holdDeadline is the declared deadline every hold-placing admit must carry
// (xs-007 row 33): an hour from now, as a job would declare.
func holdDeadline() *time.Time {
	v := time.Now().Add(time.Hour)
	return &v
}

func TestNativeCatalogRemainingProductUseCasesHTTP(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	h := New(t, ctx)
	standalone := h.StartStandalone("usd")
	token := standalone.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-product-use-cases-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	tierGroup := "saas-" + suffix
	premiumGroup := "premium-" + suffix
	premiumKey := "premium-" + suffix
	basicKey := "basic-" + suffix
	proKey := "pro-" + suffix
	movieKey := "movie-" + suffix

	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		Products: []catalog.Product{
			{
				Key:          premiumKey,
				DisplayName:  "Premium",
				TierGroup:    premiumGroup,
				Entitlements: []string{"premium"},
				Prices: []catalog.Price{{
					UnitAmount: 9_990_000,
					Currency:   "USD",
					Duration:   "30d",
					AutoRenew:  true,
				}},
			},
			{
				Key:         basicKey,
				DisplayName: "Basic",
				TierGroup:   tierGroup,
				TierRank:    intPtr(1),
				Prices: []catalog.Price{{
					UnitAmount: 19_990_000,
					Currency:   "USD",
					Duration:   "30d",
					AutoRenew:  true,
				}},
			},
			{
				Key:         proKey,
				DisplayName: "Pro",
				TierGroup:   tierGroup,
				TierRank:    intPtr(2),
				Prices: []catalog.Price{{
					UnitAmount: 49_990_000,
					Currency:   "USD",
					Duration:   "30d",
					AutoRenew:  true,
					Trial:      &catalog.PriceTrial{UnitAmount: 0, Duration: "7d"},
				}},
			},
			{
				Key:         movieKey,
				DisplayName: "Catalog Movie",
				Prices:      []catalog.Price{{UnitAmount: 4_990_000, Currency: "USD", Duration: "indefinite"}},
			},
		},
	}
	require.NoError(t, manifest.Validate())
	status, body := requestJSON(t, http.MethodPost, standalone.BaseURL+"/v1/merchant/catalog/applications", token, catalogApplicationFixture(t, standalone.BaseURL, token, manifest))
	require.Equal(t, http.StatusOK, status, string(body))

	applier := standalone.Client(openrails.WithAPIKey(token))
	premium := mustCatalogProduct(t, ctx, applier, premiumKey)
	basic := mustCatalogProduct(t, ctx, applier, basicKey)
	pro := mustCatalogProduct(t, ctx, applier, proKey)
	movie := mustCatalogProduct(t, ctx, applier, movieKey)

	require.Contains(t, premium.EntitlementsSpec, "premium")
	require.Equal(t, tierGroup, *basic.TierGroup)
	require.Equal(t, 1, basic.TierRank)
	require.Equal(t, tierGroup, *pro.TierGroup)
	require.Equal(t, 2, pro.TierRank)

	proPrices, err := catalogPrices(ctx, applier, sdkProductID(t, pro.ID), true)
	require.NoError(t, err)
	require.Len(t, proPrices, 1)
	require.True(t, proPrices[0].AutoRenew)
	require.NotNil(t, proPrices[0].AccessDurationHours)
	require.Equal(t, 720, *proPrices[0].AccessDurationHours)
	require.NotNil(t, proPrices[0].TrialUnitAmount)
	require.Equal(t, int64(0), *proPrices[0].TrialUnitAmount)
	require.NotNil(t, proPrices[0].TrialDurationHours)
	require.Equal(t, 168, *proPrices[0].TrialDurationHours)

	moviePrices, err := catalogPrices(ctx, applier, sdkProductID(t, movie.ID), true)
	require.NoError(t, err)
	require.Len(t, moviePrices, 1)
	require.Nil(t, moviePrices[0].AccessDurationHours)
	require.False(t, moviePrices[0].AutoRenew)

	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	customerID := uuid.New()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, customerID.String())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.grants WHERE merchant_id = $1 AND customer_id = $2 AND event <> 'grant'", dbtest.TestMerchantID.UUID(), customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.grants WHERE merchant_id = $1 AND customer_id = $2", dbtest.TestMerchantID.UUID(), customerID)
	})

	grantLedger := grants.New(dbtest.Queries(pool), dbtest.TestMerchantID.UUID())

	pastEnd := time.Now().Add(-24 * time.Hour)
	premiumProduct := sdkProductID(t, premium.ID).UUID()
	firstSub, err := grantLedger.Grant(ctx, grants.GrantInput{
		Customer: customerID,
		Product:  &premiumProduct,
		Kind:     grants.Entitlement,
		Source:   grants.Subscription,
		SourceID: uuid.NewString(),
		Spec:     &grants.Spec{Entitlements: []string{"premium"}},
		StartsAt: time.Now().Add(-48 * time.Hour),
		EndsAt:   &pastEnd,
	})
	require.NoError(t, err)
	require.NoError(t, grantLedger.MaterializeGrant(ctx, firstSub))
	futureEnd := time.Now().Add(30 * 24 * time.Hour)
	renewal, err := grantLedger.Grant(ctx, grants.GrantInput{
		Customer: customerID,
		Product:  &premiumProduct,
		Kind:     grants.Entitlement,
		Source:   grants.Subscription,
		SourceID: uuid.NewString(),
		Spec:     &grants.Spec{Entitlements: []string{"premium"}},
		StartsAt: time.Now().Add(-time.Hour),
		EndsAt:   &futureEnd,
	})
	require.NoError(t, err)
	require.NoError(t, grantLedger.MaterializeGrant(ctx, renewal))
	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/customers/"+customerID.String()+"/entitlements", token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var entitlementRows []struct {
		Entitlement string `json:"entitlement"`
	}
	require.NoError(t, json.Unmarshal(body, &entitlementRows))
	require.Len(t, entitlementRows, 1)
	require.Equal(t, "premium", entitlementRows[0].Entitlement)

	movieProduct := sdkProductID(t, movie.ID).UUID()
	ownership, err := grantLedger.Grant(ctx, grants.GrantInput{
		Customer: customerID,
		Product:  &movieProduct,
		Kind:     grants.Ownership,
		Source:   grants.Purchase,
		SourceID: uuid.NewString(),
	})
	require.NoError(t, err)
	require.NoError(t, grantLedger.MaterializeGrant(ctx, ownership))
	require.Equal(t, 1, liveOwnershipGrantCount(t, ctx, pool, customerID, sdkProductID(t, movie.ID).UUID()))
}
