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
	if err := c.do(ctx, http.MethodPost, "/v1/admin/catalog/applications", document, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCatalogRevision reads the catalog revision, which every catalog write
// advances, and whether catalog writes are accepted.
func (c *Client) GetCatalogRevision(ctx context.Context, requestOptions ...RequestOption) (*billing.CatalogRevision, error) {
	var out billing.CatalogRevision
	if err := c.do(ctx, http.MethodGet, "/v1/admin/catalog/revision", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RefreshCatalogDrift reads every linked PSP's catalog now and records the
// drift it finds; it changes neither the PSPs nor the catalog.
func (c *Client) RefreshCatalogDrift(ctx context.Context, requestOptions ...RequestOption) (*billing.CatalogDriftRefresh, error) {
	var out billing.CatalogDriftRefresh
	if err := c.do(ctx, http.MethodPost, "/v1/admin/catalog/drift/refresh", struct{}{}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// AskCatalog answers a question about the catalog with a model that reads it
// and may draft price changes for a person to review; it changes nothing. The
// deployment must enable it.
func (c *Client) AskCatalog(ctx context.Context, params billing.AskCatalogParams, requestOptions ...RequestOption) (*billing.CatalogAnswer, error) {
	var out billing.CatalogAnswer
	if err := c.do(ctx, http.MethodPost, "/v1/admin/catalog/ask", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
