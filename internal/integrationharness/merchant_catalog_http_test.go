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

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	openrails "github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
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
				Currency:   "usd",
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
		Status:      models.CatalogStatusActive,
	})
	require.NoError(t, err)

	plan, err = catalog.Plan(ctx, applier, &updatedManifest)
	require.NoError(t, err)
	pruned, err := catalog.ApplyWithOptions(ctx, applier, plan, catalog.ApplyOptions{Prune: true})
	require.NoError(t, err)
	require.Equal(t, 1, pruned.ProductsArchived)
	status, body := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products/"+extra.ID.String(), token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var archived billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(body, &archived))
	require.Equal(t, models.CatalogStatusArchived, archived.Status)
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
				Currency:   "usd",
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

	listURL := surface.BaseURL + "/v1/merchant/catalog/products?tier_group=" + url.QueryEscape(groupSlug) + "&active_only=true"
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
			Credits: catalog.Credits{
				"native-lifecycle-usd": {Currency: "usd", Amount: 10_000},
			},
			Prices: []catalog.Price{{
				UnitAmount: 10_000,
				Currency:   "usd",
				Duration:   "indefinite",
				Providers:  []string{},
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
			proveNativeCatalogLifecycle(t, h, surface, product.ID)
		})
	}
}

func TestNativeCatalogMeteredUsageHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	standalone := h.StartStandalone("usd")
	embedded := h.StartEmbeddedHost("usd")

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
			Key:  meterKey,
			Kind: "counter",
		}},
		Products: []catalog.Product{{
			Key:         productKey,
			DisplayName: "Metered Usage Product",
			Prices: []catalog.Price{{
				UnitAmount: 0,
				Currency:   "usd",
				Duration:   "30d",
				AutoRenew:  true,
				Providers:  []string{},
				Metered: &catalog.MeteredPrice{
					Meter:    meterKey,
					Rate:     250_000,
					PerUnits: 100,
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
	prices, err := (httpCatalogApplier{t: t, baseURL: standalone.BaseURL, token: token}).ListPricesByProduct(ctx, product.ID, true)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	var meterCount int
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = $2`, dbtest.TestMerchantID.UUID(), meterKey).Scan(&meterCount))
	require.Equal(t, 1, meterCount)
	var sidecarCount int
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_price_metered WHERE merchant_id = $1 AND price_id = $2`, dbtest.TestMerchantID.UUID(), prices[0].ID).Scan(&sidecarCount))
	require.Equal(t, 1, sidecarCount)

	for _, surface := range []*Surface{standalone, embedded} {
		t.Run(surface.Name, func(t *testing.T) {
			proveCatalogMeteredUsage(t, h, surface, prices[0].ID)
		})
	}
}

func TestNativeCatalogBundleIncludesHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	standalone := h.StartStandalone("usd")

	token := standalone.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-bundle-includes-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	childKey := "movie-" + suffix
	bundleKey := "movie-bundle-" + suffix
	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		Products: []catalog.Product{
			{
				Key:         childKey,
				DisplayName: "Included Movie",
				TierGroup:   "movies",
				Prices: []catalog.Price{{
					UnitAmount: 4_990_000,
					Currency:   "usd",
					Duration:   "indefinite",
					Providers:  []string{},
				}},
			},
			{
				Key:         bundleKey,
				DisplayName: "Movie Bundle",
				TierGroup:   "bundles",
				Includes:    []string{childKey},
				Prices: []catalog.Price{{
					UnitAmount: 9_990_000,
					Currency:   "usd",
					Duration:   "indefinite",
					Providers:  []string{},
				}},
			},
		},
	}
	require.NoError(t, manifest.Validate())
	status, body := requestJSON(t, http.MethodPost, standalone.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
		"catalog": manifest,
		"insert":  true,
	})
	require.Equal(t, http.StatusOK, status, string(body))

	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-key/"+bundleKey, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var bundle billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(body, &bundle))
	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-key/"+childKey, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var child billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(body, &child))

	dbi := dbtest.OpenAppDB(t, h.DSN)
	pool := dbi.Pool()
	customer := uuid.New()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, customer.String())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE merchant_id = $1 AND customer_id = $2 AND event <> 'grant'", dbtest.TestMerchantID.UUID(), customer)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE merchant_id = $1 AND customer_id = $2", dbtest.TestMerchantID.UUID(), customer)
	})
	grantLedger := grants.New(gen.New(pool), dbtest.TestMerchantID.UUID())
	g, err := grantLedger.Grant(ctx, grants.GrantInput{
		Customer: customer,
		Product:  &bundle.ID,
		Kind:     grants.Ownership,
		Source:   grants.Purchase,
		SourceID: "pay_" + suffix,
	})
	require.NoError(t, err)
	require.NoError(t, grantLedger.MaterializeGrant(ctx, g))
	require.Equal(t, 1, liveOwnershipGrantCount(t, ctx, pool, customer, child.ID))
	require.NoError(t, grantLedger.MaterializeGrant(ctx, g))
	require.Equal(t, 1, liveOwnershipGrantCount(t, ctx, pool, customer, child.ID))
}

func TestNativeCatalogUsageLimitBindingHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	standalone := h.StartStandalone("usd")

	token := standalone.MintAPIKey(
		dbtest.TestMerchantSlug,
		"catalog-usage-limit-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate},
	)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	productKey := "claude-plan-" + suffix
	limitKey := "claude-5x-" + suffix
	product20Key := "claude-plan-20x-" + suffix
	limit20Key := "claude-20x-" + suffix
	measure := "claude-code"
	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		UsageLimits: []catalog.UsageLimit{
			{
				Key:     limitKey,
				Measure: measure,
				Windows: []catalog.UsageLimitWindow{{
					Window: "1h",
					Amount: 100,
				}},
			},
			{
				Key:     limit20Key,
				Measure: measure,
				Windows: []catalog.UsageLimitWindow{{
					Window: "1h",
					Amount: 400,
				}},
			},
		},
		Products: []catalog.Product{
			{
				Key:         productKey,
				DisplayName: "Claude 5x Plan",
				TierGroup:   "claude-code",
				TierRank:    intPtr(1),
				UsageLimits: []string{limitKey},
				Prices: []catalog.Price{{
					UnitAmount: 20_000_000,
					Currency:   "usd",
					Duration:   "30d",
					AutoRenew:  true,
					Providers:  []string{},
				}},
			},
			{
				Key:         product20Key,
				DisplayName: "Claude 20x Plan",
				TierGroup:   "claude-code",
				TierRank:    intPtr(2),
				UsageLimits: []string{limit20Key},
				Prices: []catalog.Price{{
					UnitAmount: 80_000_000,
					Currency:   "usd",
					Duration:   "30d",
					AutoRenew:  true,
					Providers:  []string{},
				}},
			},
		},
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
	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-key/"+product20Key, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var product20 billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(body, &product20))

	dbi := dbtest.OpenAppDB(t, h.DSN)
	pool := dbi.Pool()
	customer := openrails.CustomerID(uuid.New())
	customerID := customer.UUID()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, customerID.String())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.product_usage_limit_bindings WHERE merchant_id = $1 AND customer_id = $2", dbtest.TestMerchantID.UUID(), customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE merchant_id = $1 AND customer_id = $2 AND event <> 'grant'", dbtest.TestMerchantID.UUID(), customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE merchant_id = $1 AND customer_id = $2", dbtest.TestMerchantID.UUID(), customerID)
	})
	grantLedger := grants.New(gen.New(pool), dbtest.TestMerchantID.UUID())
	g, err := grantLedger.Grant(ctx, grants.GrantInput{
		Customer: customerID,
		Product:  &product.ID,
		Kind:     grants.Entitlement,
		Source:   grants.Purchase,
		SourceID: uuid.NewString(),
		Spec:     &grants.Spec{Entitlements: []string{"claude-code"}},
	})
	require.NoError(t, err)
	require.NoError(t, grantLedger.MaterializeGrant(ctx, g))
	require.Equal(t, 1, productUsageLimitBindingCount(t, ctx, pool, customerID, limitKey, false))

	client := standalone.Client()
	depositSourceID := uuid.New()
	_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{
		CustomerID: &customer,
		Invoker:    customerID.String(),
		Currency:   "usd",
		Amount:     1_000,
		Source:     "catalog-usage-limit",
		SourceID:   &depositSourceID,
	})
	require.NoError(t, err)

	firstID := "usage-limit-first-" + uuid.NewString()
	verdicts, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{{
		CustomerID:      customerID.String(),
		Invoker:         customerID.String(),
		InvokerType:     string(identity.InvokerTypePayer),
		Resource:        measure,
		Currency:        "usd",
		EstimatedAmount: 60,
		RequestID:       firstID,
		Source:          "catalog-usage-limit",
	}})
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])

	secondID := "usage-limit-second-" + uuid.NewString()
	verdicts, err = client.AdmitBatch(ctx, []openrails.AdmitRequest{{
		CustomerID:      customerID.String(),
		Invoker:         customerID.String(),
		InvokerType:     string(identity.InvokerTypePayer),
		Resource:        measure,
		Currency:        "usd",
		EstimatedAmount: 50,
		RequestID:       secondID,
		Source:          "catalog-usage-limit",
	}})
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.False(t, verdicts[0].Allowed(), "%+v", verdicts[0])

	_, err = grantLedger.Revoke(ctx, g.ID, "refund")
	require.NoError(t, err)
	require.NoError(t, grantLedger.MaterializeGrant(ctx, g))
	require.Equal(t, 1, productUsageLimitBindingCount(t, ctx, pool, customerID, limitKey, true))
	require.Equal(t, 0, productUsageLimitBindingCount(t, ctx, pool, customerID, limitKey, false))

	g20, err := grantLedger.Grant(ctx, grants.GrantInput{
		Customer: customerID,
		Product:  &product20.ID,
		Kind:     grants.Entitlement,
		Source:   grants.Purchase,
		SourceID: uuid.NewString(),
		Spec:     &grants.Spec{Entitlements: []string{"claude-code-20x"}},
	})
	require.NoError(t, err)
	require.NoError(t, grantLedger.MaterializeGrant(ctx, g20))
	require.Equal(t, 1, productUsageLimitBindingCount(t, ctx, pool, customerID, limit20Key, false))

	upgradeID := "usage-limit-20x-" + uuid.NewString()
	verdicts, err = client.AdmitBatch(ctx, []openrails.AdmitRequest{{
		CustomerID:      customerID.String(),
		Invoker:         customerID.String(),
		InvokerType:     string(identity.InvokerTypePayer),
		Resource:        measure,
		Currency:        "usd",
		EstimatedAmount: 150,
		RequestID:       upgradeID,
		Source:          "catalog-usage-limit",
	}})
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])
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
	aiGroup := "ai-credits-" + suffix
	apiGroup := "api-credits-" + suffix
	movieGroup := "movies-" + suffix
	premiumKey := "premium-" + suffix
	basicKey := "basic-" + suffix
	proKey := "pro-" + suffix
	aiSlug := "ai-credits-" + suffix
	apiSlug := "api-credits-" + suffix
	movieKey := "movie-" + suffix
	aiUnitName := "ai-image-credit-" + suffix
	apiUnitName := "fal-api-credit-" + suffix
	aiUnit := dbtest.TestMerchantSlug + "/" + aiUnitName
	apiUnit := dbtest.TestMerchantSlug + "/" + apiUnitName

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
					Currency:   "usd",
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
					Currency:   "usd",
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
					Currency:   "usd",
					Duration:   "30d",
					AutoRenew:  true,
					Trial:      &catalog.PriceTrial{UnitAmount: 0, Duration: "7d"},
				}},
			},
			{
				Key:         aiSlug,
				DisplayName: "AI Image Credits",
				TierGroup:   aiGroup,
				Credits: catalog.Credits{
					"ai-image-gen": {Unit: aiUnit, Amount: 100},
				},
				Prices: []catalog.Price{{UnitAmount: 5_000_000, Currency: "usd", Duration: "indefinite"}},
			},
			{
				Key:         apiSlug,
				DisplayName: "fal.ai API Credits",
				TierGroup:   apiGroup,
				Credits: catalog.Credits{
					"fal-api": {Unit: apiUnit, Amount: 2_000},
				},
				Prices: []catalog.Price{{UnitAmount: 20_000_000, Currency: "usd", Duration: "indefinite"}},
			},
			{
				Key:         movieKey,
				DisplayName: "Catalog Movie",
				TierGroup:   movieGroup,
				Prices:      []catalog.Price{{UnitAmount: 4_990_000, Currency: "usd", Duration: "indefinite"}},
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
	aiProduct := mustCatalogProduct(t, ctx, applier, aiSlug)
	apiProduct := mustCatalogProduct(t, ctx, applier, apiSlug)
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

	dbi := dbtest.OpenAppDB(t, h.DSN)
	pool := dbi.Pool()
	customerID := uuid.New()
	customer := identity.CustomerID(customerID)
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, customerID.String())
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE merchant_id = $1 AND customer_id = $2 AND event <> 'grant'", dbtest.TestMerchantID.UUID(), customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.grants WHERE merchant_id = $1 AND customer_id = $2", dbtest.TestMerchantID.UUID(), customerID)
	})

	moneySvc := money.NewMoneyService(dbi)
	_, err = moneySvc.DefineCustomCreditType(ctx, aiUnitName, 0)
	require.NoError(t, err)
	_, err = moneySvc.DefineCustomCreditType(ctx, apiUnitName, 0)
	require.NoError(t, err)

	require.NoError(t, moneySvc.GrantPurchaseCredits(ctx, money.GrantPurchaseCreditsParams{
		Payer:     customer,
		PaymentID: uuid.New(),
		Spec:      serviceCreditsToModel(aiProduct.CreditsSpec),
		Source:    "catalog-ai-credit-purchase",
	}))
	require.NoError(t, moneySvc.GrantPurchaseCredits(ctx, money.GrantPurchaseCreditsParams{
		Payer:     customer,
		PaymentID: uuid.New(),
		Spec:      serviceCreditsToModel(apiProduct.CreditsSpec),
		Source:    "catalog-api-credit-purchase",
	}))

	client := standalone.Client()
	aiBalance, err := client.GetCreditAccount(ctx, customerID.String(), aiUnit)
	require.NoError(t, err)
	require.Equal(t, int64(100), aiBalance.BalanceAmount)
	grantLedger := grants.New(gen.New(pool), dbtest.TestMerchantID.UUID())
	require.NoError(t, grantLedger.CreditSpend(ctx, customerID, aiUnit, 40, customerID.String(), "ai-image-generation", "catalog-ai-image-generation", uuid.NewString()))
	aiBalance, err = client.GetCreditAccount(ctx, customerID.String(), aiUnit)
	require.NoError(t, err)
	require.Equal(t, int64(60), aiBalance.BalanceAmount)
	apiBalance, err := client.GetCreditAccount(ctx, customerID.String(), apiUnit)
	require.NoError(t, err)
	require.Equal(t, int64(2_000), apiBalance.BalanceAmount)

	pastEnd := time.Now().Add(-24 * time.Hour)
	firstSub, err := grantLedger.Grant(ctx, grants.GrantInput{
		Customer: customerID,
		Product:  &premium.ID,
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
		Product:  &premium.ID,
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

	ownership, err := grantLedger.Grant(ctx, grants.GrantInput{
		Customer: customerID,
		Product:  &movie.ID,
		Kind:     grants.Ownership,
		Source:   grants.Purchase,
		SourceID: uuid.NewString(),
	})
	require.NoError(t, err)
	require.NoError(t, grantLedger.MaterializeGrant(ctx, ownership))
	require.Equal(t, 1, liveOwnershipGrantCount(t, ctx, pool, customerID, movie.ID))
}

func proveCatalogMeteredUsage(t *testing.T, h *Harness, surface *Surface, priceID uuid.UUID) {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, h.DSN)
	pool := dbi.Pool()
	payerID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payerID)
	})

	status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/usage/metered", surface.Token, map[string]any{
		"customer_id": payerID.String(),
		"currency":    "usd",
		"price_id":    priceID.String(),
		"source_id":   "period-" + surface.Name + "-" + uuid.NewString(),
		"aggregate":   420,
	})
	require.Equal(t, http.StatusOK, status, string(body))
	var rated struct {
		Amount int64 `json:"amount"`
	}
	require.NoError(t, json.Unmarshal(body, &rated))
	require.Equal(t, int64(1_050_000), rated.Amount)

	inv, err := money.NewMoneyService(dbi).FinalizeInvoice(ctx, identity.CustomerID(payerID), money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
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

func productUsageLimitBindingCount(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, customer uuid.UUID, key string, revoked bool) int {
	t.Helper()
	cond := "revoked_at IS NULL"
	if revoked {
		cond = "revoked_at IS NOT NULL"
	}
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.product_usage_limit_bindings WHERE merchant_id = $1 AND customer_id = $2 AND usage_limit_key = $3 AND `+cond,
		dbtest.TestMerchantID.UUID(), customer, key).Scan(&n))
	return n
}

func mustCatalogProduct(t *testing.T, ctx context.Context, applier httpCatalogApplier, key string) billingservice.CatalogProduct {
	t.Helper()
	product, err := applier.GetProductByKey(ctx, key)
	require.NoError(t, err)
	return *product
}

func serviceCreditsToModel(in billingservice.CreditsSpec) models.CreditsSpec {
	out := make(models.CreditsSpec, len(in))
	for key, spec := range in {
		out[key] = models.CreditGrantSpec{
			Unit:        spec.Unit,
			Amount:      spec.Amount,
			ExpiryHours: spec.ExpiryHours,
			Cadence:     models.CreditGrantCadence(spec.Cadence),
		}
	}
	return out
}

func proveNativeCatalogLifecycle(t *testing.T, h *Harness, surface *Surface, productID uuid.UUID) {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, h.DSN)
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
	depositSourceID := uuid.New()
	_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{
		CustomerID: &payer,
		Invoker:    payerID.String(),
		Currency:   "usd",
		Amount:     10_000,
		Source:     "catalog-native-lifecycle",
		SourceID:   &depositSourceID,
	})
	require.NoError(t, err)
	balance, err := client.Balance(ctx, payerID.String())
	require.NoError(t, err)
	require.Equal(t, int64(10_000), balance.BalanceAmount)

	requestID := "native-lifecycle-" + surface.Name + "-" + uuid.NewString()
	verdicts, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{{
		CustomerID:      payerID.String(),
		Invoker:         payerID.String(),
		InvokerType:     string(identity.InvokerTypePayer),
		Resource:        "vm-small",
		Currency:        "usd",
		EstimatedAmount: 2_500,
		RequestID:       requestID,
		Source:          "native-lifecycle",
	}})
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])
	require.NoError(t, client.Capture(ctx, requestID, 2_000, &openrails.CaptureUsage{
		EventType: "vm-runtime",
		Resource:  "vm-small",
		Metadata:  map[string]any{"tier": "basic"},
		Source:    "native-lifecycle",
		SourceID:  requestID,
	}))

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)

	rows, err := client.UsageRollup(ctx, payerID.String(), "usd", from, to, "resource")
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
		req.Header.Set("Authorization", "Bearer "+token)
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

func (a httpCatalogApplier) ListProducts(_ context.Context, opts billingservice.ListProductsOptions) ([]billingservice.CatalogProduct, int64, error) {
	q := url.Values{}
	if opts.TierGroup != "" {
		q.Set("tier_group", opts.TierGroup)
	}
	if opts.ActiveOnly {
		q.Set("active_only", "true")
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
		return nil, 0, fmt.Errorf("list products: status %d: %s", status, string(body))
	}
	var page struct {
		Items []billingservice.CatalogProduct `json:"items"`
		Total int64                           `json:"total"`
	}
	require.NoError(a.t, json.Unmarshal(body, &page))
	return page.Items, page.Total, nil
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

func (a httpCatalogApplier) UpdateProduct(_ context.Context, id uuid.UUID, req billingservice.UpdateProductRequest) (*billingservice.CatalogProduct, error) {
	status, body := requestJSON(a.t, http.MethodPatch, a.baseURL+"/v1/merchant/catalog/products/"+id.String(), a.token, req)
	if status != http.StatusOK {
		return nil, fmt.Errorf("update product: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogProduct
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) DeactivateProduct(_ context.Context, id uuid.UUID) (*billingservice.CatalogProduct, error) {
	status, body := requestJSON(a.t, http.MethodPost, a.baseURL+"/v1/merchant/catalog/products/"+id.String()+"/deactivate", a.token, nil)
	if status != http.StatusOK {
		return nil, fmt.Errorf("deactivate product: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogProduct
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) ListPricesByProduct(_ context.Context, productID uuid.UUID, activeOnly bool) ([]billingservice.CatalogPrice, error) {
	q := url.Values{"product_id": []string{productID.String()}}
	if activeOnly {
		q.Set("active_only", "true")
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

func (a httpCatalogApplier) ActivatePrice(_ context.Context, id uuid.UUID) (*billingservice.CatalogPrice, error) {
	status, body := requestJSON(a.t, http.MethodPost, a.baseURL+"/v1/merchant/catalog/prices/"+id.String()+"/activate", a.token, nil)
	if status != http.StatusOK {
		return nil, fmt.Errorf("activate price: status %d: %s", status, string(body))
	}
	var out billingservice.CatalogPrice
	require.NoError(a.t, json.Unmarshal(body, &out))
	return &out, nil
}

func (a httpCatalogApplier) DeactivatePrice(_ context.Context, id uuid.UUID) (*billingservice.CatalogPrice, error) {
	status, body := requestJSON(a.t, http.MethodPost, a.baseURL+"/v1/merchant/catalog/prices/"+id.String()+"/deactivate", a.token, nil)
	if status != http.StatusOK {
		return nil, fmt.Errorf("deactivate price: status %d: %s", status, string(body))
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
	Merchant    string               `yaml:"merchant"`
	Products    []catalog.Product    `yaml:"products"`
	Meters      []catalog.Meter      `yaml:"meters"`
	UsageLimits []catalog.UsageLimit `yaml:"usage_limits"`
}

func loadExampleCatalogForHTTP(t *testing.T) catalog.Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.example.yaml"))
	require.NoError(t, err)
	var file exampleCatalogFile
	require.NoError(t, yaml.UnmarshalWithOptions(raw, &file, yaml.DisallowUnknownField()))
	require.Equal(t, catalog.SupportedVersion, file.Version)
	require.Len(t, file.Catalogs, 1)

	entry := file.Catalogs[0]
	suffix := "-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	meterKeys := map[string]string{}
	for i := range entry.Meters {
		old := entry.Meters[i].Key
		entry.Meters[i].Key += suffix
		meterKeys[old] = entry.Meters[i].Key
	}
	limitKeys := map[string]string{}
	for i := range entry.UsageLimits {
		old := entry.UsageLimits[i].Key
		entry.UsageLimits[i].Key += suffix
		limitKeys[old] = entry.UsageLimits[i].Key
	}
	for i := range entry.Products {
		entry.Products[i].Key += suffix
		if entry.Products[i].TierGroup != "" {
			entry.Products[i].TierGroup += suffix
		}
		for j := range entry.Products[i].UsageLimits {
			entry.Products[i].UsageLimits[j] = limitKeys[entry.Products[i].UsageLimits[j]]
		}
		for j := range entry.Products[i].Includes {
			entry.Products[i].Includes[j] += suffix
		}
		for j := range entry.Products[i].Prices {
			entry.Products[i].Prices[j].Providers = nil
			entry.Products[i].Prices[j].ProviderLinks = nil
			if mp := entry.Products[i].Prices[j].Metered; mp != nil {
				mp.Meter = meterKeys[mp.Meter]
			}
		}
	}
	m := catalog.Manifest{
		Version:     file.Version,
		Products:    entry.Products,
		Meters:      entry.Meters,
		UsageLimits: entry.UsageLimits,
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
		`SELECT count(*) FROM openrails.catalog_usage_limits WHERE merchant_id = $1 AND key = ANY($2::text[])`,
		dbtest.TestMerchantID.UUID(), exampleUsageLimitKeys(m)).Scan(&n))
	require.Equal(t, len(m.UsageLimits), n)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.catalog_meters WHERE merchant_id = $1 AND key = ANY($2::text[])`,
		dbtest.TestMerchantID.UUID(), exampleMeterKeys(m)).Scan(&n))
	require.Equal(t, len(m.Meters), n)

	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM openrails.product_includes pi
JOIN openrails.products p ON p.id = pi.product_id
WHERE p.merchant_id = $1 AND p.key = ANY($2::text[])`, dbtest.TestMerchantID.UUID(), keys).Scan(&n))
	require.Equal(t, exampleIncludesCount(m), n)

	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM openrails.product_usage_limits pul
JOIN openrails.products p ON p.id = pul.product_id
WHERE p.merchant_id = $1 AND p.key = ANY($2::text[])`, dbtest.TestMerchantID.UUID(), keys).Scan(&n))
	require.Equal(t, exampleProductUsageLimitCount(m), n)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.catalog_price_metered WHERE merchant_id = $1 AND meter_key = ANY($2::text[])`,
		dbtest.TestMerchantID.UUID(), exampleMeterKeys(m)).Scan(&n))
	require.Equal(t, exampleMeteredPriceCount(m), n)

	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM openrails.prices pr
JOIN openrails.products p ON p.id = pr.product_id
WHERE p.merchant_id = $1 AND p.key = ANY($2::text[]) AND pr.trial_unit_amount = 0 AND pr.trial_duration_hours = 168`,
		dbtest.TestMerchantID.UUID(), keys).Scan(&n))
	require.Equal(t, 1, n)
}

func exampleProductKeys(m catalog.Manifest) []string {
	keys := make([]string, 0, len(m.Products))
	for _, p := range m.Products {
		keys = append(keys, p.Key)
	}
	return keys
}

func exampleUsageLimitKeys(m catalog.Manifest) []string {
	keys := make([]string, 0, len(m.UsageLimits))
	for _, limit := range m.UsageLimits {
		keys = append(keys, limit.Key)
	}
	return keys
}

func exampleMeterKeys(m catalog.Manifest) []string {
	keys := make([]string, 0, len(m.Meters))
	for _, meter := range m.Meters {
		keys = append(keys, meter.Key)
	}
	return keys
}

func exampleIncludesCount(m catalog.Manifest) int {
	var n int
	for _, p := range m.Products {
		n += len(p.Includes)
	}
	return n
}

func exampleProductUsageLimitCount(m catalog.Manifest) int {
	var n int
	for _, p := range m.Products {
		n += len(p.UsageLimits)
	}
	return n
}

func exampleMeteredPriceCount(m catalog.Manifest) int {
	var n int
	for _, p := range m.Products {
		for _, price := range p.Prices {
			if price.Metered != nil {
				n++
			}
		}
	}
	return n
}
