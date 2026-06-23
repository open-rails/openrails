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

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/catalog"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

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
		TierGroups: []catalog.TierGroup{{
			Slug: groupSlug,
			Products: []catalog.Product{{
				Slug:        productSlug,
				DisplayName: "Plan Product",
				Description: "inserted through HTTP-backed catalog apply",
				TierRank:    1,
				Prices: []catalog.Price{{
					UnitAmount:    1299,
					Currency:      "usd",
					Interval:      "month",
					IntervalCount: 1,
				}},
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
	updatedManifest.TierGroups = []catalog.TierGroup{{
		Slug: groupSlug,
		Products: []catalog.Product{{
			Slug:        productSlug,
			DisplayName: "Plan Product Updated",
			Description: "updated through HTTP-backed catalog apply",
			TierRank:    2,
			Prices:      manifest.TierGroups[0].Products[0].Prices,
		}},
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
		TierGroups: []catalog.TierGroup{{
			Slug: groupSlug,
			Products: []catalog.Product{{
				Slug:        productSlug,
				DisplayName: "Publish Product",
				Description: "published through the live merchant catalog route",
				TierRank:    1,
				Prices: []catalog.Price{{
					UnitAmount:    1499,
					Currency:      "usd",
					Interval:      "month",
					IntervalCount: 1,
				}},
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
