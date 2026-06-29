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
	"strings"
	"testing"
	"time"

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

	productSlug := "catalog-route-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	createStatus, createBody := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/products", catalogToken, map[string]any{
		"slug":         productSlug,
		"display_name": "Catalog Route Product",
		"description":  "created through the live merchant catalog route",
	})
	require.Equal(t, http.StatusCreated, createStatus, string(createBody))
	var created struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	require.NoError(t, json.Unmarshal(createBody, &created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, productSlug, created.Slug)

	getStatus, getBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products/by-slug/"+productSlug, catalogToken, nil)
	require.Equal(t, http.StatusOK, getStatus, string(getBody))
	var fetched struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	require.NoError(t, json.Unmarshal(getBody, &fetched))
	require.Equal(t, created.ID, fetched.ID)
	require.Equal(t, productSlug, fetched.Slug)

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
	productSlug := "plan-product-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	manifest := &catalog.Manifest{
		Version: catalog.SupportedVersion,
		Products: []catalog.Product{{
			Key:         productSlug,
			DisplayName: "Plan Product",
			Description: "inserted through HTTP-backed catalog apply",
			TierGroup:   groupSlug,
			TierRank:    intPtr(1),
			Prices: []catalog.Price{{
				UnitAmount: 1299,
				Currency:   "usd",
				Interval:   "month",
			}},
		}},
	}
	require.NoError(t, manifest.Validate())

	plan, err := catalog.Plan(ctx, applier, manifest)
	require.NoError(t, err)
	_, err = catalog.ApplyWithOptions(ctx, applier, plan, catalog.ApplyOptions{})
	require.NoError(t, err)
	_, err = applier.GetProductBySlug(ctx, productSlug)
	require.Error(t, err, "bare apply options must be plan-only over HTTP")

	plan, err = catalog.Plan(ctx, applier, manifest)
	require.NoError(t, err)
	inserted, err := catalog.ApplyWithOptions(ctx, applier, plan, catalog.ApplyOptions{Insert: true})
	require.NoError(t, err)
	require.Equal(t, 1, inserted.ProductsCreated)
	require.Equal(t, 1, inserted.PricesCreated)
	product, err := applier.GetProductBySlug(ctx, productSlug)
	require.NoError(t, err)
	require.Equal(t, "Plan Product", product.DisplayName)

	updatedManifest := *manifest
	updatedManifest.TierGroups = nil
	updatedManifest.Products = []catalog.Product{{
		Key:         productSlug,
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
	product, err = applier.GetProductBySlug(ctx, productSlug)
	require.NoError(t, err)
	require.Equal(t, "Plan Product Updated", product.DisplayName)
	require.Equal(t, 2, product.TierRank)

	extraSlug := "prune-extra-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	extra, err := applier.CreateProduct(ctx, billingservice.CreateProductRequest{
		Slug:        extraSlug,
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
	productSlug := "publish-product-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		Products: []catalog.Product{{
			Key:         productSlug,
			DisplayName: "Publish Product",
			Description: "published through the live merchant catalog route",
			TierGroup:   groupSlug,
			TierRank:    intPtr(1),
			Prices: []catalog.Price{{
				UnitAmount: 1499,
				Currency:   "usd",
				Interval:   "month",
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
		require.NotEqual(t, productSlug, item.Slug)
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

	foundStatus, foundBody := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/products/by-slug/"+productSlug, token, nil)
	require.Equal(t, http.StatusOK, foundStatus, string(foundBody))
	var product billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(foundBody, &product))
	require.Equal(t, productSlug, product.Slug)
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

	productSlug := "native-lifecycle-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		Products: []catalog.Product{{
			Key:          productSlug,
			DisplayName:  "Native Lifecycle Product",
			Description:  "published catalog anchor for native lifecycle proof",
			Entitlements: []string{"native-lifecycle-premium"},
			Credits: catalog.Credits{
				"native-lifecycle-usd": {Currency: "usd", Amount: 10_000},
			},
			Prices: []catalog.Price{{
				UnitAmount: 10_000,
				Currency:   "usd",
				Interval:   "once",
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

	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-slug/"+productSlug, token, nil)
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
	productSlug := "metered-usage-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	meterKey := "vm-seconds-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		Meters: []catalog.Meter{{
			Key:  meterKey,
			Kind: "counter",
		}},
		Products: []catalog.Product{{
			Key:         productSlug,
			DisplayName: "Metered Usage Product",
			Prices: []catalog.Price{{
				UnitAmount: 0,
				Currency:   "usd",
				Interval:   "month",
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

	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-slug/"+productSlug, token, nil)
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
	childSlug := "movie-" + suffix
	bundleSlug := "movie-bundle-" + suffix
	manifest := catalog.Manifest{
		Version: catalog.SupportedVersion,
		Products: []catalog.Product{
			{
				Key:         childSlug,
				DisplayName: "Included Movie",
				TierGroup:   "movies",
				Prices: []catalog.Price{{
					UnitAmount: 4_990_000,
					Currency:   "usd",
					Interval:   "once",
					Providers:  []string{},
				}},
			},
			{
				Key:         bundleSlug,
				DisplayName: "Movie Bundle",
				TierGroup:   "bundles",
				Includes:    []string{childSlug},
				Prices: []catalog.Price{{
					UnitAmount: 9_990_000,
					Currency:   "usd",
					Interval:   "once",
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

	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-slug/"+bundleSlug, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var bundle billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(body, &bundle))
	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-slug/"+childSlug, token, nil)
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
	productSlug := "claude-plan-" + suffix
	limitKey := "claude-5x-" + suffix
	product20Slug := "claude-plan-20x-" + suffix
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
				Key:         productSlug,
				DisplayName: "Claude 5x Plan",
				TierGroup:   "claude-code",
				TierRank:    intPtr(1),
				UsageLimits: []string{limitKey},
				Prices: []catalog.Price{{
					UnitAmount: 20_000_000,
					Currency:   "usd",
					Interval:   "month",
					Providers:  []string{},
				}},
			},
			{
				Key:         product20Slug,
				DisplayName: "Claude 20x Plan",
				TierGroup:   "claude-code",
				TierRank:    intPtr(2),
				UsageLimits: []string{limit20Key},
				Prices: []catalog.Price{{
					UnitAmount: 80_000_000,
					Currency:   "usd",
					Interval:   "month",
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

	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-slug/"+productSlug, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var product billingservice.CatalogProduct
	require.NoError(t, json.Unmarshal(body, &product))
	status, body = requestJSON(t, http.MethodGet, standalone.BaseURL+"/v1/merchant/catalog/products/by-slug/"+product20Slug, token, nil)
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

func (a httpCatalogApplier) GetProductBySlug(_ context.Context, slug string) (*billingservice.CatalogProduct, error) {
	status, body := requestJSON(a.t, http.MethodGet, a.baseURL+"/v1/merchant/catalog/products/by-slug/"+url.PathEscape(slug), a.token, nil)
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("product not found: %s", slug)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("get product by slug: status %d: %s", status, string(body))
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
