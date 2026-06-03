package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	billingservice "github.com/open-rails/openrails/pkg/service"
)

// HTTPApplier drives a remote standalone OpenRails over its admin catalog HTTP
// API (the same routes the in-process facade backs: POST/GET/PATCH on
// /admin/catalog/products and /admin/catalog/prices). Auth is an operator-admin
// bearer token (OperatorAdminRequired). It implements the same Applier
// interface as the in-process adapter, so plan/apply are mode-agnostic.
//
// BaseURL must include any API prefix the server mounts admin routes under, up
// to but not including "/admin" — e.g. "https://billing.example.com/billing/v1".
type HTTPApplier struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// Compile-time guarantee that HTTPApplier satisfies Applier.
var _ Applier = (*HTTPApplier)(nil)

// NewHTTPApplier builds an HTTP-mode Applier targeting baseURL with the given
// operator-admin bearer token.
func NewHTTPApplier(baseURL, token string) *HTTPApplier {
	return &HTTPApplier{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// paginated mirrors handlers.paginatedResponse[T].
type paginated[T any] struct {
	Items  []T   `json:"items"`
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

func (h *HTTPApplier) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.BaseURL+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (h *HTTPApplier) GetProductBySlug(ctx context.Context, slug string) (*billingservice.CatalogProduct, error) {
	var out billingservice.CatalogProduct
	status, err := h.do(ctx, http.MethodGet, "/admin/catalog/products/by-slug/"+url.PathEscape(slug), nil, &out)
	if err != nil {
		if status == http.StatusNotFound {
			// Mirror the in-process "not found" contract so Plan treats it as create.
			return nil, fmt.Errorf("product not found: %s", slug)
		}
		return nil, err
	}
	return &out, nil
}

func (h *HTTPApplier) ListProducts(ctx context.Context, opts billingservice.ListProductsOptions) ([]billingservice.CatalogProduct, int64, error) {
	q := url.Values{}
	if opts.ActiveOnly {
		q.Set("active_only", "true")
	}
	if strings.TrimSpace(opts.TierGroup) != "" {
		q.Set("tier_group", opts.TierGroup)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	var out paginated[billingservice.CatalogProduct]
	if _, err := h.do(ctx, http.MethodGet, "/admin/catalog/products?"+q.Encode(), nil, &out); err != nil {
		return nil, 0, err
	}
	return out.Items, out.Total, nil
}

func (h *HTTPApplier) CreateProduct(ctx context.Context, req billingservice.CreateProductRequest) (*billingservice.CatalogProduct, error) {
	var out billingservice.CatalogProduct
	if _, err := h.do(ctx, http.MethodPost, "/admin/catalog/products", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (h *HTTPApplier) UpdateProduct(ctx context.Context, id uuid.UUID, req billingservice.UpdateProductRequest) (*billingservice.CatalogProduct, error) {
	var out billingservice.CatalogProduct
	if _, err := h.do(ctx, http.MethodPatch, "/admin/catalog/products/"+id.String(), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (h *HTTPApplier) DeactivateProduct(ctx context.Context, id uuid.UUID) (*billingservice.CatalogProduct, error) {
	var out billingservice.CatalogProduct
	if _, err := h.do(ctx, http.MethodPost, "/admin/catalog/products/"+id.String()+"/deactivate", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (h *HTTPApplier) ListPricesByProduct(ctx context.Context, productID uuid.UUID, activeOnly bool) ([]billingservice.CatalogPrice, error) {
	q := url.Values{}
	q.Set("product_id", productID.String())
	if activeOnly {
		q.Set("active_only", "true")
	}
	var out paginated[billingservice.CatalogPrice]
	if _, err := h.do(ctx, http.MethodGet, "/admin/catalog/prices?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (h *HTTPApplier) CreatePrice(ctx context.Context, req billingservice.CreatePriceRequest) (*billingservice.CatalogPrice, error) {
	var out billingservice.CatalogPrice
	if _, err := h.do(ctx, http.MethodPost, "/admin/catalog/prices", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (h *HTTPApplier) ActivatePrice(ctx context.Context, id uuid.UUID) (*billingservice.CatalogPrice, error) {
	var out billingservice.CatalogPrice
	if _, err := h.do(ctx, http.MethodPost, "/admin/catalog/prices/"+id.String()+"/activate", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (h *HTTPApplier) DeactivatePrice(ctx context.Context, id uuid.UUID) (*billingservice.CatalogPrice, error) {
	var out billingservice.CatalogPrice
	if _, err := h.do(ctx, http.MethodPost, "/admin/catalog/prices/"+id.String()+"/deactivate", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
