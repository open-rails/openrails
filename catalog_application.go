package openrails

import (
	"context"
	"net/http"
	"net/url"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// ApplyCatalog applies a catalog document as the apply manager. Each product,
// price key and meter in it applies whole, unless a field it names was set
// differently by an edit: that object is skipped and listed in the receipt's
// Conflicts, the rest applies, and the document is not recorded as applied,
// so applying it again retries the skipped objects. params.Force overwrites
// those fields instead. A document that applied whole is remembered by its
// canonical content hash: applying it again returns the original receipt even
// after later catalog edits. Config.Catalog uses the same behavior, never
// forced.
func (c *Client) ApplyCatalog(ctx context.Context, document *catalog.Application, params billing.ApplyCatalogParams, requestOptions ...RequestOption) (*billing.CatalogApplicationReceipt, error) {
	if document == nil {
		return nil, invalidErr("catalog document is required")
	}
	if err := document.Validate(); err != nil {
		return nil, invalidErr(err.Error())
	}
	path := "/v1/admin/catalog/applications"
	if params.Force {
		path = withQuery(path, url.Values{"force": {"true"}})
	}
	var out billing.CatalogApplicationReceipt
	if err := c.do(ctx, http.MethodPost, path, document, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCatalogRevision reads the catalog revision, which every catalog write
// advances.
func (c *Client) GetCatalogRevision(ctx context.Context, requestOptions ...RequestOption) (*billing.CatalogRevision, error) {
	var out billing.CatalogRevision
	if err := c.do(ctx, http.MethodGet, "/v1/admin/catalog/revision", nil, &out, requestOptions...); err != nil {
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
