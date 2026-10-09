package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// ApplyCatalog applies one partial merchant catalog batch atomically. The server
// remembers its canonical content hash permanently: retries return the original
// receipt even after later catalog changes. Config.Catalog uses the same behavior.
func (c *Client) ApplyCatalog(ctx context.Context, document *catalog.Application, requestOptions ...RequestOption) (*billing.CatalogApplicationReceipt, error) {
	if document == nil {
		return nil, invalidErr("catalog document is required")
	}
	if err := document.Validate(); err != nil {
		return nil, invalidErr(err.Error())
	}
	var out billing.CatalogApplicationReceipt
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalog/applications", document, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReplaceEntitlements moves every product granting each pair's From key to
// grant its To key instead, in one catalog edit; an empty To removes From.
// Holders follow their products: they lose From and gain To at once. The
// receipt lists each changed product.
func (c *Client) ReplaceEntitlements(ctx context.Context, params billing.ReplaceEntitlementsParams, requestOptions ...RequestOption) (*billing.CatalogApplicationReceipt, error) {
	if len(params.Pairs) == 0 {
		return nil, invalidErr("pairs is required")
	}
	var out billing.CatalogApplicationReceipt
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalog/entitlement-replacements", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCatalogRevision reads the catalog revision, which every catalog write
// advances, and whether catalog writes are accepted.
func (c *Client) GetCatalogRevision(ctx context.Context, requestOptions ...RequestOption) (*billing.CatalogRevision, error) {
	var out billing.CatalogRevision
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/revision", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCatalogDrift returns one page of open findings that a PSP's copy of the
// catalog differs from OpenRails, newest first.
func (c *Client) ListCatalogDrift(ctx context.Context, params billing.CatalogDriftListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.CatalogDrift], error) {
	q := pageValues(nil, params.PageRequest)
	for name, value := range map[string]string{"rail": params.Rail, "kind": params.Kind, "resource_type": params.ResourceType} {
		if value != "" {
			q.Set(name, value)
		}
	}
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.CatalogDrift]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/drift?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RefreshCatalogDrift reads every linked PSP's catalog now and records the
// drift it finds; it changes neither the PSPs nor the catalog.
func (c *Client) RefreshCatalogDrift(ctx context.Context, requestOptions ...RequestOption) (*billing.CatalogDriftRefresh, error) {
	var out billing.CatalogDriftRefresh
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalog/drift/refresh", struct{}{}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// AskCatalog answers a question about the catalog with a model that reads it
// and may draft price changes for a person to review; it changes nothing. The
// deployment must enable it.
func (c *Client) AskCatalog(ctx context.Context, params billing.AskCatalogParams, requestOptions ...RequestOption) (*billing.CatalogAnswer, error) {
	var out billing.CatalogAnswer
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalog/ask", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
