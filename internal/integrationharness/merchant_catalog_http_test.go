//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails"

	"github.com/open-rails/openrails/internal/testauth"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/catalog"
)

func intPtr(v int) *int { return &v }

func TestStandaloneMerchantCatalogRoutesHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	catalogToken := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-writer-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)
	readOnlyToken := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-denied-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCustomerSettingsRead},
	)

	productKey := "catalog-route-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	createStatus, createBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/products", catalogToken, map[string]any{
		"key":          productKey,
		"display_name": "Catalog Route Product",
		"description":  "created through the live merchant catalog route",
	})
	require.Equal(t, http.StatusCreated, createStatus, string(createBody))
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	require.NoError(t, json.Unmarshal(createBody, &created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, productKey, created.Key)

	// Retired fields are refused in the coded envelope: the field is named in
	// param and Go's decoder text never reaches the wire.
	requireRetiredFieldRefused := func(status int, body []byte, field string) {
		t.Helper()
		require.Equal(t, http.StatusBadRequest, status, string(body))
		var refused struct {
			Error openrails.ErrorDetails `json:"error"`
		}
		require.NoError(t, json.Unmarshal(body, &refused), string(body))
		require.Equal(t, api.ErrorTypeInvalidRequest, refused.Error.Type, string(body))
		require.Equal(t, api.CodeInvalidParam, refused.Error.Code, string(body))
		require.NotNil(t, refused.Error.Param, string(body))
		require.Equal(t, field, *refused.Error.Param, string(body))
		require.Equal(t, "unknown field "+field, refused.Error.Message, string(body))
		require.NotEmpty(t, refused.Error.RequestID, string(body))
	}
	for _, retired := range []string{"credits_spec", "set_credits"} {
		status, body := requestJSON(t, http.MethodPatch, surface.BaseURL+"/v1/merchant/catalog/products/"+created.ID, catalogToken, map[string]any{retired: true})
		requireRetiredFieldRefused(status, body, retired)
	}
	for _, retired := range []string{"credits", "includes", "usage_limits"} {
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", catalogToken, map[string]any{
			"catalog": map[string]any{"version": 1, "products": []any{map[string]any{"key": "retired", "display_name": "Retired", retired: []any{}}}},
			"insert":  true,
		})
		requireRetiredFieldRefused(status, body, retired)
	}
	wrongTypeStatus, wrongTypeBody := requestJSON(t, http.MethodPatch, surface.BaseURL+"/v1/merchant/catalog/products/"+created.ID, catalogToken, map[string]any{"display_name": 7})
	require.Equal(t, http.StatusBadRequest, wrongTypeStatus, string(wrongTypeBody))
	require.Contains(t, string(wrongTypeBody), `"code":"invalid_param"`)
	require.Contains(t, string(wrongTypeBody), `"param":"display_name"`)
	require.NotContains(t, string(wrongTypeBody), "json:")

	getStatus, getBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products/by-key/"+productKey, catalogToken, nil)
	require.Equal(t, http.StatusOK, getStatus, string(getBody))
	var fetched struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	require.NoError(t, json.Unmarshal(getBody, &fetched))
	require.Equal(t, created.ID, fetched.ID)
	require.Equal(t, productKey, fetched.Key)

	unauthStatus, unauthBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products", "", nil)
	require.Equal(t, http.StatusUnauthorized, unauthStatus, string(unauthBody))

	deniedStatus, deniedBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products", readOnlyToken, nil)
	require.Equal(t, http.StatusForbidden, deniedStatus, string(deniedBody))
}

func TestStandaloneMerchantCatalogApplyOptionsOverHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	token := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-apply-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)
	applier := httpCatalogApplier{t: t, baseURL: surface.BaseURL, token: token}

	groupSlug := "apply-flags-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	productKey := "plan-product-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	manifest := &catalog.Manifest{
		Version: catalog.SupportedVersion,
		Products: []catalog.Product{{
			Key:         productKey,
			DisplayName: "Plan Product",
			Description: "inserted through HTTP-backed catalog apply",
			TierGroup:   groupSlug,
			TierRank:    intPtr(1),
			Prices: []catalog.Price{{
				UnitAmount: 1299,
				Currency:   "USD",
				Duration:   "30d",
				AutoRenew:  true,
			}},
		}},
	}
	require.NoError(t, manifest.Validate())

	plan, err := catalog.Plan(ctx, applier, manifest)
	require.NoError(t, err)
	_, err = catalog.ApplyWithOptions(ctx, applier, plan, catalog.ApplyOptions{})
	require.NoError(t, err)
	_, err = applier.GetProductByKey(ctx, productKey)
	require.Error(t, err, "bare apply options must be plan-only over HTTP")

	plan, err = catalog.Plan(ctx, applier, manifest)
	require.NoError(t, err)
	inserted, err := catalog.ApplyWithOptions(ctx, applier, plan, catalog.ApplyOptions{Insert: true})
	require.NoError(t, err)
	require.Equal(t, 1, inserted.ProductsCreated)
	require.Equal(t, 1, inserted.PricesCreated)
	product, err := applier.GetProductByKey(ctx, productKey)
	require.NoError(t, err)
	require.Equal(t, "Plan Product", product.DisplayName)

	updatedManifest := *manifest
	updatedManifest.TierGroups = nil
	updatedManifest.Products = []catalog.Product{{
		Key:         productKey,
		DisplayName: "Plan Product Updated",
		Description: "updated through HTTP-backed catalog apply",
		TierGroup:   groupSlug,
		TierRank:    intPtr(2),
		Prices:      manifest.Products[0].Prices,
	}}
	require.NoError(t, updatedManifest.Validate())
	plan, err = catalog.Plan(ctx, applier, &updatedManifest)
	require.NoError(t, err)
	updated, err := catalog.ApplyWithOptions(ctx, applier, plan, catalog.ApplyOptions{Overwrite: true})
	require.NoError(t, err)
	require.Equal(t, 1, updated.ProductsUpdated)
	product, err = applier.GetProductByKey(ctx, productKey)
	require.NoError(t, err)
	require.Equal(t, "Plan Product Updated", product.DisplayName)
	require.Equal(t, 2, product.TierRank)

	extraSlug := "prune-extra-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	extra, err := applier.CreateProduct(ctx, billingservice.CreateProductRequest{
		Key:         extraSlug,
		DisplayName: "Prune Extra",
		TierGroup:   &groupSlug,
		Archived:    false,
	})
	require.NoError(t, err)

	plan, err = catalog.Plan(ctx, applier, &updatedManifest)
	require.NoError(t, err)
	pruned, err := catalog.ApplyWithOptions(ctx, applier, plan, catalog.ApplyOptions{Prune: true})
	require.NoError(t, err)
	require.Equal(t, 1, pruned.ProductsArchived)
	status, body := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products/"+openrails.ProductID(extra.ID).String(), token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var archived billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(body, &archived))
	require.True(t, archived.Archived)
}

func TestStandaloneMerchantCatalogPublishHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	token := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-publish-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)
	deniedToken := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-publish-denied-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead},
	)

	groupSlug := "publish-group-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	productKey := "publish-product-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		Products: []catalog.Product{{
			Key:         productKey,
			DisplayName: "Publish Product",
			Description: "published through the live merchant catalog route",
			TierGroup:   groupSlug,
			TierRank:    intPtr(1),
			Prices: []catalog.Price{{
				UnitAmount: 1499,
				Currency:   "USD",
				Duration:   "30d",
				AutoRenew:  true,
			}},
		}},
	}
	require.NoError(t, manifest.Validate())

	deniedStatus, deniedBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", deniedToken, map[string]any{
		"catalog": manifest,
		"insert":  true,
	})
	require.Equal(t, http.StatusForbidden, deniedStatus, string(deniedBody))

	planStatus, planBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
		"catalog": manifest,
	})
	require.Equal(t, http.StatusOK, planStatus, string(planBody))
	var planned struct {
		Plan   *catalog.ApplyPlan   `json:"plan"`
		Result *catalog.ApplyResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(planBody, &planned))
	require.NotNil(t, planned.Plan)
	require.Nil(t, planned.Result)

	listURL := surface.BaseURL + "/v1/merchant/catalog/products?tier_group=" + url.QueryEscape(groupSlug) + "&archived=false"
	missingStatus, missingBody := requestJSON(t, http.MethodGet, listURL, token, nil)
	require.Equal(t, http.StatusOK, missingStatus, string(missingBody))
	var missingPage struct {
		Items []billingservice.CatalogProduct `json:"items"`
		Total int64                           `json:"total"`
	}
	require.NoError(t, json.Unmarshal(missingBody, &missingPage))
	for _, item := range missingPage.Items {
		require.NotEqual(t, productKey, item.Key)
	}

	applyStatus, applyBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
		"catalog": manifest,
		"insert":  true,
	})
	require.Equal(t, http.StatusOK, applyStatus, string(applyBody))
	var applied struct {
		Plan   *catalog.ApplyPlan   `json:"plan"`
		Result *catalog.ApplyResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(applyBody, &applied))
	require.NotNil(t, applied.Plan)
	require.NotNil(t, applied.Result)
	require.Equal(t, 1, applied.Result.ProductsCreated)
	require.Equal(t, 1, applied.Result.PricesCreated)

	foundStatus, foundBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products/by-key/"+productKey, token, nil)
	require.Equal(t, http.StatusOK, foundStatus, string(foundBody))
	var product billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(foundBody, &product))
	require.Equal(t, productKey, product.Key)
}

// TestStandaloneMerchantCatalogPriceKeyVersionBumpHTTP is #774's MODE 1
// YAML-edit+push round-trip through the FULL converge (POST .../catalog/
// publish drives the identical catalog.Plan+catalog.ApplyWithOptions pipeline
// the manifest CLI uses): an amount edit under the auto-defaulted key version-
// bumps (new row, key re-pointed, old row archived), and flip-flopping back
// to the original amount REACTIVATES the same original row rather than
// minting a third.
func TestStandaloneMerchantCatalogPriceKeyVersionBumpHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	token := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-price-key-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)

	productKey := "price-key-bump-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	wantKey := productKey + "-monthly"
	manifestAt := func(amount int64) catalog.Manifest {
		return catalog.Manifest{
			Version: catalog.SupportedVersion,
			Products: []catalog.Product{{
				Key:         productKey,
				DisplayName: "Price Key Bump Product",
				Prices: []catalog.Price{{
					UnitAmount: amount,
					Currency:   "USD",
					Duration:   "30d",
					AutoRenew:  true,
				}},
			}},
		}
	}
	publish := func(m catalog.Manifest) *catalog.ApplyResult {
		t.Helper()
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
			"catalog": m, "insert": true, "overwrite": true, "prune": true,
		})
		require.Equal(t, http.StatusOK, status, string(body))
		var out struct {
			Result *catalog.ApplyResult `json:"result"`
		}
		require.NoError(t, json.Unmarshal(body, &out))
		require.NotNil(t, out.Result)
		return out.Result
	}
	getByKey := func() billingservice.CatalogPrice {
		t.Helper()
		status, body := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/prices/by-key/"+wantKey, token, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var p billingservice.CatalogPrice
		require.NoError(t, json.Unmarshal(body, &p))
		return p
	}

	// v1: create at $10/mo — auto-defaults to "<product-key>-monthly".
	result := publish(manifestAt(1000000))
	require.Equal(t, 1, result.ProductsCreated)
	require.Equal(t, 1, result.PricesCreated)
	original := getByKey()
	require.Equal(t, wantKey, original.Key)
	require.EqualValues(t, 1000000, original.UnitAmount)

	// v2: amount edit under the SAME (auto-defaulted) key -> version bump.
	result = publish(manifestAt(1200000))
	require.Equal(t, 1, result.PricesCreated, "new substance -> new row")
	require.Equal(t, 1, result.PricesArchived, "displaced row archived")
	bumped := getByKey()
	require.Equal(t, wantKey, bumped.Key)
	require.EqualValues(t, 1200000, bumped.UnitAmount)
	require.NotEqual(t, original.ID, bumped.ID)

	oldStatus, oldBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/prices/"+openrails.PriceID(original.ID).String(), token, nil)
	require.Equal(t, http.StatusOK, oldStatus, string(oldBody))
	var oldRow billingservice.CatalogPrice
	require.NoError(t, json.Unmarshal(oldBody, &oldRow))
	require.True(t, oldRow.Archived, "displaced row is archived, not deleted")
	require.Equal(t, wantKey, oldRow.Key, "archived predecessor keeps the key as a back-reference")

	// v3: flip back to $10 -> REACTIVATES the original row (never a third).
	result = publish(manifestAt(1000000))
	require.Equal(t, 0, result.PricesCreated, "flip-flop must reactivate, not create")
	require.Equal(t, 1, result.PricesActivated)
	require.Equal(t, 1, result.PricesArchived)
	reactivated := getByKey()
	require.Equal(t, original.ID, reactivated.ID, "reactivated the SAME row (#662 deterministic id)")

	pricesStatus, pricesBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/prices?product_id="+openrails.ProductID(bumped.ProductID).String(), token, nil)
	require.Equal(t, http.StatusOK, pricesStatus, string(pricesBody))
	var page struct {
		Items []billingservice.CatalogPrice `json:"items"`
	}
	require.NoError(t, json.Unmarshal(pricesBody, &page))
	require.Len(t, page.Items, 2, "flip-flopping forever yields exactly two rows total")
}

func TestExampleCatalogPublishesOverHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	token := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-example-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)

	manifest := loadExampleCatalogForHTTP(t)
	expectedProducts, expectedPrices := catalogShapeCounts(manifest)

	planStatus, planBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
		"catalog": manifest,
	})
	require.Equal(t, http.StatusOK, planStatus, string(planBody))
	var planned struct {
		Plan   *catalog.ApplyPlan   `json:"plan"`
		Result *catalog.ApplyResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(planBody, &planned))
	require.NotNil(t, planned.Plan)
	require.Nil(t, planned.Result)
	require.Equal(t, expectedProducts, countProductActions(planned.Plan, catalog.ProductCreate))
	require.Equal(t, expectedPrices, countPriceActions(planned.Plan, catalog.PriceCreate))
	require.Zero(t, exampleProductCount(t, ctx, h, manifest))

	applyStatus, applyBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
		"catalog": manifest,
		"insert":  true,
	})
	require.Equal(t, http.StatusOK, applyStatus, string(applyBody))
	var applied struct {
		Plan   *catalog.ApplyPlan   `json:"plan"`
		Result *catalog.ApplyResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(applyBody, &applied))
	require.NotNil(t, applied.Result)
	require.Equal(t, expectedProducts, applied.Result.ProductsCreated)
	require.Equal(t, expectedPrices, applied.Result.PricesCreated)

	assertExampleCatalogRows(t, ctx, h, manifest, expectedProducts, expectedPrices)

	againStatus, againBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
		"catalog": manifest,
	})
	require.Equal(t, http.StatusOK, againStatus, string(againBody))
	var again struct {
		Plan *catalog.ApplyPlan `json:"plan"`
	}
	require.NoError(t, json.Unmarshal(againBody, &again))
	require.Zero(t, countProductActions(again.Plan, catalog.ProductCreate))
	require.Zero(t, countPriceActions(again.Plan, catalog.PriceCreate))
}

// TestCatalogPublishRateCardsHTTP drives the full manifest -> apply -> DB path for
// the #638/#639 rate-card model: a usage product priced by a matrix rate card
// (and no flat prices) and a variable credit-purchase product, published over
// HTTP, must create the products AND persist their rate-card / credit-purchase
// sidecars. Guards the applier mapping that the spec-level sidecar test skips.
func TestCatalogPublishRateCardsHTTP(t *testing.T) {
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
		_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = $2", mid, meterKey)
		_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2", mid, meterKey)
		_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE merchant_id = $1 AND key = ANY($2::text[])", mid, []string{dropletKey})
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

	applyStatus, applyBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
		"catalog": manifest,
		"insert":  true,
	})
	require.Equal(t, http.StatusOK, applyStatus, string(applyBody))

	// The matrix rate card persisted and links its meter + product.
	var model, rcMeter string
	require.NoError(t, h.Pool().QueryRow(ctx, `
SELECT rc.price ->> 'model', rc.meter_key
FROM openrails.catalog_rate_cards rc
JOIN openrails.products p ON p.id = rc.product_id
WHERE p.merchant_id = $1 AND p.key = $2`, mid, dropletKey).Scan(&model, &rcMeter))
	require.Equal(t, "per_unit", model)
	require.Equal(t, meterKey, rcMeter)

	// The rate-card meter persisted with its aggregation.
	var agg string
	require.NoError(t, h.Pool().QueryRow(ctx, `
SELECT aggregation FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2`, mid, meterKey).Scan(&agg))
	require.Equal(t, "sum", agg)
}

// or#896: a `trial:` first phase declared on a rail that cannot execute one
// (NMI, Solana) is REFUSED at publish, naming the limitation — it used to be
// accepted, dropped, and the subscriber charged the full amount immediately.
// The rails that can execute one (Stripe, CCBill) still publish.
func TestCatalogPublishRefusesTrialOnRailsWithoutFirstPhase(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
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
			_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.prices WHERE merchant_id = $1 AND product_id IN (SELECT id FROM openrails.products WHERE merchant_id = $1 AND key = $2)", mid, productKey)
			_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE merchant_id = $1 AND key = $2", mid, productKey)
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
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
			"catalog": manifest,
			"insert":  true,
		})
		return status, body, productKey
	}

	for _, psp := range []string{"nmi", "solana"} {
		t.Run(psp+" is refused", func(t *testing.T) {
			status, body, productKey := publish(t, psp)
			require.Equal(t, http.StatusBadRequest, status, string(body))
			require.Contains(t, string(body), "trial")
			require.Contains(t, string(body), psp)
			require.Contains(t, string(body), "silently dropped")

			// The refusal is total: no price row was written for the product.
			var priceCount int
			require.NoError(t, h.Pool().QueryRow(ctx, `
SELECT count(*) FROM openrails.prices pr
JOIN openrails.products p ON p.id = pr.product_id
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
SELECT pr.trial_unit_amount, pr.trial_duration_hours FROM openrails.prices pr
JOIN openrails.products p ON p.id = pr.product_id
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
	status, body := requestJSON(t, http.MethodPost, standalone.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
		"catalog": manifest,
		"insert":  true,
	})
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
	status, body := requestJSON(t, http.MethodPost, standalone.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
		"catalog": manifest,
		"insert":  true,
	})
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
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2`, dbtest.TestMerchantID.UUID(), meterKey).Scan(&meterCount))
	require.Equal(t, 1, meterCount)
	var unitAmount, divideBy int64
	require.NoError(t, h.Pool().QueryRow(ctx, `
SELECT (price -> 'per_unit' ->> 'unit_amount')::bigint, (price -> 'per_unit' ->> 'divide_by')::bigint
FROM openrails.catalog_rate_cards
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
		_, _ = pool.Exec(mctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(mctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(mctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payerID)
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
SELECT count(*) FROM openrails.grants g
WHERE g.merchant_id = $1
  AND g.customer_id = $2
  AND g.product_id = $3
  AND g.kind = 'ownership'
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants t
      WHERE t.merchant_id = g.merchant_id
        AND t.supersedes_id = g.id
        AND t.event IN ('revoke', 'expire', 'supersede')
  )`, dbtest.TestMerchantID.UUID(), customer, product).Scan(&n))
	return n
}

func mustCatalogProduct(t *testing.T, ctx context.Context, applier httpCatalogApplier, key string) billingservice.CatalogProduct {
	t.Helper()
	product, err := applier.GetProductByKey(ctx, key)
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
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payerID)
	})

	grantLedger := grants.New(gen.New(pool), dbtest.TestMerchantID.UUID())
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
		CustomerID: &payer,
		Invoker:    payerID.String(),
		Currency:   "USD",
		Amount:     10_000,
		Source:     "catalog-native-lifecycle",
		SourceID:   depositSourceID,
	})
	require.NoError(t, err)
	balance, err := client.Balance(ctx, openrails.CustomerID(payerID))
	require.NoError(t, err)
	require.Equal(t, int64(10_000), balance.BalanceAmount)

	requestID := "native-lifecycle-" + surface.Name + "-" + uuid.NewString()
	verdicts, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{{
		CustomerID:      openrails.CustomerID(payerID),
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

	rows, err := client.UsageRollup(ctx, openrails.CustomerID(payerID), "usd", from, to, "resource")
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

func (a httpCatalogApplier) GetProductByKey(_ context.Context, key string) (*billingservice.CatalogProduct, error) {
	status, body := requestJSON(a.t, http.MethodGet, a.baseURL+"/v1/merchant/catalog/products/by-key/"+url.PathEscape(key), a.token, nil)
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("product not found: %s", key)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("get product by key: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogProduct
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) ListProducts(_ context.Context, opts billingservice.ListProductsOptions) (billingservice.CatalogPage[billingservice.CatalogProduct], error) {
	q := url.Values{}
	if opts.TierGroup != "" {
		q.Set("tier_group", opts.TierGroup)
	}
	if opts.Archived != nil {
		q.Set("archived", fmt.Sprint(*opts.Archived))
	}
	if opts.Limit > 0 {
		q.Set("limit", fmt.Sprint(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", fmt.Sprint(opts.Offset))
	}
	u := a.baseURL + "/v1/merchant/catalog/products"
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	status, body := requestJSON(a.t, http.MethodGet, u, a.token, nil)
	if status != http.StatusOK {
		return billingservice.CatalogPage[billingservice.CatalogProduct]{}, fmt.Errorf("list products: status %d: %s", status, string(body))
	}
	var page billingservice.CatalogPage[billingservice.CatalogProduct]
	require.NoError(a.t, json.Unmarshal(body, &page))
	return page, nil
}

func (a httpCatalogApplier) CreateProduct(_ context.Context, req billingservice.CreateProductRequest) (*billingservice.CatalogProduct, error) {
	status, body := requestJSON(a.t, http.MethodPost, a.baseURL+"/v1/merchant/catalog/products", a.token, req)
	if status != http.StatusCreated {
		return nil, fmt.Errorf("create product: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogProduct
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) UpdateProduct(_ context.Context, id openrails.ProductID, req billingservice.UpdateProductRequest) (*billingservice.CatalogProduct, error) {
	status, body := requestJSON(a.t, http.MethodPatch, a.baseURL+"/v1/merchant/catalog/products/"+openrails.ProductID(id).String(), a.token, req)
	if status != http.StatusOK {
		return nil, fmt.Errorf("update product: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogProduct
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) DeactivateProduct(_ context.Context, id openrails.ProductID) (*billingservice.CatalogProduct, error) {
	status, body := requestJSON(a.t, http.MethodPost, a.baseURL+"/v1/merchant/catalog/products/"+openrails.ProductID(id).String()+"/deactivate", a.token, nil)
	if status != http.StatusOK {
		return nil, fmt.Errorf("deactivate product: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogProduct
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) ListPricesByProduct(_ context.Context, productID openrails.ProductID, activeOnly bool) ([]billingservice.CatalogPrice, error) {
	q := url.Values{"product_id": []string{productID.String()}}
	if activeOnly {
		q.Set("archived", "false")
	}
	status, body := requestJSON(a.t, http.MethodGet, a.baseURL+"/v1/merchant/catalog/prices?"+q.Encode(), a.token, nil)
	if status != http.StatusOK {
		return nil, fmt.Errorf("list prices: status %d: %s", status, string(body))
	}
	var page struct {
		Items []billingservice.CatalogPrice `json:"items"`
	}
	require.NoError(a.t, json.Unmarshal(body, &page))
	return page.Items, nil
}

func (a httpCatalogApplier) CreatePrice(_ context.Context, req billingservice.CreatePriceRequest) (*billingservice.CatalogPrice, error) {
	status, body := requestJSON(a.t, http.MethodPost, a.baseURL+"/v1/merchant/catalog/prices", a.token, req)
	if status != http.StatusCreated {
		return nil, fmt.Errorf("create price: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogPrice
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) UpdatePrice(_ context.Context, id openrails.PriceID, req billingservice.UpdatePriceRequest) (*billingservice.CatalogPrice, error) {
	status, body := requestJSON(a.t, http.MethodPatch, a.baseURL+"/v1/merchant/catalog/prices/"+openrails.PriceID(id).String(), a.token, req)
	if status != http.StatusOK {
		return nil, fmt.Errorf("update price: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogPrice
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) ActivatePrice(_ context.Context, id openrails.PriceID) (*billingservice.CatalogPrice, error) {
	status, body := requestJSON(a.t, http.MethodPost, a.baseURL+"/v1/merchant/catalog/prices/"+openrails.PriceID(id).String()+"/activate", a.token, nil)
	if status != http.StatusOK {
		return nil, fmt.Errorf("activate price: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogPrice
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) DeactivatePrice(_ context.Context, id openrails.PriceID) (*billingservice.CatalogPrice, error) {
	status, body := requestJSON(a.t, http.MethodPost, a.baseURL+"/v1/merchant/catalog/prices/"+openrails.PriceID(id).String()+"/deactivate", a.token, nil)
	if status != http.StatusOK {
		return nil, fmt.Errorf("deactivate price: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogPrice
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) SetPriceKey(_ context.Context, id openrails.PriceID, key string) (*billingservice.CatalogPrice, error) {
	status, body := requestJSON(a.t, http.MethodPost, a.baseURL+"/v1/merchant/catalog/prices/"+openrails.PriceID(id).String()+"/key", a.token, map[string]string{"key": key})
	if status != http.StatusOK {
		return nil, fmt.Errorf("set price key: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogPrice
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

type exampleCatalogFile struct {
	Version  int                   `yaml:"version"`
	Catalogs []exampleCatalogEntry `yaml:"catalogs"`
}

type exampleCatalogEntry struct {
	Merchant string            `yaml:"merchant"`
	Products []catalog.Product `yaml:"products"`
	Meters   []catalog.Meter   `yaml:"meters"`
}

func loadExampleCatalogForHTTP(t *testing.T) catalog.Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.example.yaml"))
	require.NoError(t, err)
	var file exampleCatalogFile
	require.NoError(t, yaml.UnmarshalWithOptions(raw, &file, yaml.DisallowUnknownField()))
	require.Equal(t, catalog.SupportedVersion, file.Version)
	require.GreaterOrEqual(t, len(file.Catalogs), 1)

	// The example is multi-merchant; exercise the HTTP apply path against the
	// anthropic catalog (subscription prices and entitlements, which the
	// applier fully supports). Rate-card apply gets its own
	// test when the applier persists rate_cards (#638).
	var entry exampleCatalogEntry
	for _, c := range file.Catalogs {
		if c.Merchant == "anthropic" {
			entry = c
		}
	}
	require.Equal(t, "anthropic", entry.Merchant, "example must include the anthropic catalog")
	suffix := "-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	meterKeys := map[string]string{}
	for i := range entry.Meters {
		old := entry.Meters[i].Key
		entry.Meters[i].Key += suffix
		meterKeys[old] = entry.Meters[i].Key
	}
	for i := range entry.Products {
		entry.Products[i].Key += suffix
		if entry.Products[i].TierGroup != "" {
			entry.Products[i].TierGroup += suffix
		}
		for j := range entry.Products[i].Prices {
			entry.Products[i].Prices[j].PSPs = nil
			entry.Products[i].Prices[j].PSPLinks = nil
		}
		for j := range entry.Products[i].RateCards {
			if mapped, ok := meterKeys[entry.Products[i].RateCards[j].Meter]; ok {
				entry.Products[i].RateCards[j].Meter = mapped
			}
		}
	}
	m := catalog.Manifest{
		Version:  file.Version,
		Products: entry.Products,
		Meters:   entry.Meters,
	}
	require.NoError(t, m.Validate())
	return m
}

func catalogShapeCounts(m catalog.Manifest) (products int, prices int) {
	for _, p := range m.Products {
		products++
		prices += len(p.Prices)
	}
	return products, prices
}

func countProductActions(plan *catalog.ApplyPlan, action catalog.ProductAction) int {
	if plan == nil {
		return 0
	}
	var n int
	for _, g := range plan.Groups {
		for _, p := range g.Products {
			if p.Action == action {
				n++
			}
		}
	}
	return n
}

func countPriceActions(plan *catalog.ApplyPlan, action catalog.PriceAction) int {
	if plan == nil {
		return 0
	}
	var n int
	for _, g := range plan.Groups {
		for _, p := range g.Products {
			for _, price := range p.Prices {
				if price.Action == action {
					n++
				}
			}
		}
	}
	return n
}

func exampleProductCount(t *testing.T, ctx context.Context, h *Harness, m catalog.Manifest) int {
	t.Helper()
	var n int
	require.NoError(t, h.Pool().QueryRow(ctx,
		`SELECT count(*) FROM openrails.products WHERE merchant_id = $1 AND key = ANY($2::text[])`,
		dbtest.TestMerchantID.UUID(), exampleProductKeys(m)).Scan(&n))
	return n
}

func assertExampleCatalogRows(t *testing.T, ctx context.Context, h *Harness, m catalog.Manifest, expectedProducts, expectedPrices int) {
	t.Helper()
	pool := h.Pool()
	keys := exampleProductKeys(m)
	require.Equal(t, expectedProducts, exampleProductCount(t, ctx, h, m))

	var n int
	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM openrails.prices pr
JOIN openrails.products p ON p.id = pr.product_id
WHERE p.merchant_id = $1 AND p.key = ANY($2::text[])`, dbtest.TestMerchantID.UUID(), keys).Scan(&n))
	require.Equal(t, expectedPrices, n)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = ANY($2::text[])`,
		dbtest.TestMerchantID.UUID(), exampleMeterKeys(m)).Scan(&n))
	require.Equal(t, len(m.Meters), n)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.catalog_rate_cards WHERE merchant_id = $1 AND meter_key = ANY($2::text[])`,
		dbtest.TestMerchantID.UUID(), exampleMeterKeys(m)).Scan(&n))
	require.Equal(t, exampleUsageRateCardCount(m), n)

	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM openrails.prices pr
JOIN openrails.products p ON p.id = pr.product_id
WHERE p.merchant_id = $1 AND p.key = ANY($2::text[]) AND pr.trial_unit_amount = 0 AND pr.trial_duration_hours = 168`,
		dbtest.TestMerchantID.UUID(), keys).Scan(&n))
	require.Equal(t, exampleFreeTrialPriceCount(m), n)
}

func exampleProductKeys(m catalog.Manifest) []string {
	keys := make([]string, 0, len(m.Products))
	for _, p := range m.Products {
		keys = append(keys, p.Key)
	}
	return keys
}

// exampleFreeTrialPriceCount counts prices in the published manifest that carry a
// free 7-day trial — data-driven so the assertion holds for whichever merchant
// slice the test publishes (anthropic has none), not a stale hardcoded 1.
func exampleFreeTrialPriceCount(m catalog.Manifest) int {
	n := 0
	for _, p := range m.Products {
		for _, pr := range p.Prices {
			if pr.Trial != nil && pr.Trial.UnitAmount == 0 && pr.Trial.Duration == "7d" {
				n++
			}
		}
	}
	return n
}

func exampleMeterKeys(m catalog.Manifest) []string {
	keys := make([]string, 0, len(m.Meters))
	for _, meter := range m.Meters {
		keys = append(keys, meter.Key)
	}
	return keys
}

// exampleUsageRateCardCount counts the usage rate cards in the published
// manifest. or#893: declared rate_cards are the only source — the metered:
// price sugar that used to translate into one is gone.
func exampleUsageRateCardCount(m catalog.Manifest) int {
	var n int
	for _, p := range m.Products {
		for _, rc := range p.RateCards {
			if rc.Meter != "" {
				n++
			}
		}
	}
	return n
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
	status, body := requestJSON(t, http.MethodPost, standalone.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
		"catalog": manifest,
		"insert":  true,
	})
	require.Equal(t, http.StatusOK, status, string(body))

	applier := httpCatalogApplier{t: t, baseURL: standalone.BaseURL, token: token}
	premium := mustCatalogProduct(t, ctx, applier, premiumKey)
	basic := mustCatalogProduct(t, ctx, applier, basicKey)
	pro := mustCatalogProduct(t, ctx, applier, proKey)
	movie := mustCatalogProduct(t, ctx, applier, movieKey)

	require.Contains(t, premium.EntitlementsSpec, "premium")
	require.Equal(t, tierGroup, *basic.TierGroup)
	require.Equal(t, 1, basic.TierRank)
	require.Equal(t, tierGroup, *pro.TierGroup)
	require.Equal(t, 2, pro.TierRank)

	proPrices, err := applier.ListPricesByProduct(ctx, pro.ID, true)
	require.NoError(t, err)
	require.Len(t, proPrices, 1)
	require.True(t, proPrices[0].AutoRenew)
	require.NotNil(t, proPrices[0].AccessDurationHours)
	require.Equal(t, 720, *proPrices[0].AccessDurationHours)
	require.NotNil(t, proPrices[0].TrialUnitAmount)
	require.Equal(t, int64(0), *proPrices[0].TrialUnitAmount)
	require.NotNil(t, proPrices[0].TrialDurationHours)
	require.Equal(t, 168, *proPrices[0].TrialDurationHours)

	moviePrices, err := applier.ListPricesByProduct(ctx, movie.ID, true)
	require.NoError(t, err)
	require.Len(t, moviePrices, 1)
	require.Nil(t, moviePrices[0].AccessDurationHours)
	require.False(t, moviePrices[0].AutoRenew)

	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	customerID := uuid.New()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, customerID.String())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE merchant_id = $1 AND customer_id = $2 AND event <> 'grant'", dbtest.TestMerchantID.UUID(), customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE merchant_id = $1 AND customer_id = $2", dbtest.TestMerchantID.UUID(), customerID)
	})

	grantLedger := grants.New(gen.New(pool), dbtest.TestMerchantID.UUID())

	pastEnd := time.Now().Add(-24 * time.Hour)
	premiumProduct := premium.ID.UUID()
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

	movieProduct := movie.ID.UUID()
	ownership, err := grantLedger.Grant(ctx, grants.GrantInput{
		Customer: customerID,
		Product:  &movieProduct,
		Kind:     grants.Ownership,
		Source:   grants.Purchase,
		SourceID: uuid.NewString(),
	})
	require.NoError(t, err)
	require.NoError(t, grantLedger.MaterializeGrant(ctx, ownership))
	require.Equal(t, 1, liveOwnershipGrantCount(t, ctx, pool, customerID, movie.ID.UUID()))
}
