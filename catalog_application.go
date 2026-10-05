package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// ApplyCatalog applies a catalog document. A declarative document (no
// application ID or expected revision) converges the catalog to its contents
// and replays while nothing has changed since. A guarded one replays by ID and
// applies only at its expected revision. While Config.Catalog declares the
// catalog it is refused (billing.ErrCatalogDeclared).
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
	var out billing.ListPage[billing.CatalogDrift]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/drift?"+q.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckCatalogDrift reads every linked PSP's catalog now and records the
// drift it finds; it changes neither the PSPs nor the catalog.
func (c *Client) CheckCatalogDrift(ctx context.Context, requestOptions ...RequestOption) (*billing.CatalogDriftCheck, error) {
	var out billing.CatalogDriftCheck
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
