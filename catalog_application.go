package openrails

import (
	"context"
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/pkg/catalog"
)

type CatalogApplyParams = catalog.Application
type CatalogApplyProduct = catalog.ApplyProduct
type CatalogApplyPrice = catalog.ApplyPrice
type CatalogApplyMeter = catalog.ApplyMeter
type CatalogField[T any] = catalog.Field[T]

func CatalogValue[T any](v T) CatalogField[T] { return catalog.Value(v) }
func CatalogNull[T any]() CatalogField[T]     { return catalog.Null[T]() }

type CatalogApplicationReceipt struct {
	ApplicationID   string `json:"application_id"`
	CatalogID       string `json:"catalog_id"`
	BaseRevision    int64  `json:"base_revision"`
	AppliedRevision int64  `json:"applied_revision"`
	Replayed        bool   `json:"replayed"`
	ProductsChanged int    `json:"products_changed"`
	PricesChanged   int    `json:"prices_changed"`
}

type CatalogRevision struct {
	Revision      int64 `json:"revision"`
	WritesAllowed bool  `json:"writes_allowed"`
}
type CatalogClient struct{ client *Client }

// Apply commits one authorized batch; retries must keep the original identity
// and payload. A new application ID deliberately applies the declaration again.
func (c *CatalogClient) Apply(ctx context.Context, params *CatalogApplyParams) (*CatalogApplicationReceipt, error) {
	if c.client.ownCatalog {
		return nil, fmt.Errorf("catalog batch applications require merchant catalog authority")
	}
	if params == nil {
		return nil, fmt.Errorf("catalog application is required")
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	var out CatalogApplicationReceipt
	if err := c.client.do(ctx, http.MethodPost, "/v1/merchant/catalog/applications", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Revision is a read-only merchant precondition, available with updates disabled.
func (c *CatalogClient) Revision(ctx context.Context) (*CatalogRevision, error) {
	if c.client.ownCatalog {
		return nil, fmt.Errorf("catalog revision requires merchant catalog authority")
	}
	var out CatalogRevision
	if err := c.client.do(ctx, http.MethodGet, "/v1/merchant/catalog/revision", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func ParseCatalogApplicationYAML(raw []byte) (*CatalogApplyParams, error) {
	return catalog.ParseApplicationYAML(raw)
}
