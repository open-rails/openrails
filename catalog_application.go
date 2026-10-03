package openrails

import (
	"context"
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

type CatalogClient struct{ client *Client }

// Apply commits one authorized batch; retries must keep the original identity
// and payload. A new application ID deliberately applies the declaration again.
func (c *CatalogClient) Apply(ctx context.Context, params *billing.CatalogApplyParams, requestOptions ...RequestOption) (*billing.CatalogApplicationReceipt, error) {
	if c.client.ownCatalog {
		return nil, fmt.Errorf("catalog batch applications require merchant catalog authority")
	}
	if params == nil {
		return nil, fmt.Errorf("catalog application is required")
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	var out billing.CatalogApplicationReceipt
	if err := c.client.do(ctx, http.MethodPost, "/v1/merchant/catalog/applications", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// Revision is a read-only merchant precondition, available with updates disabled.
func (c *CatalogClient) Revision(ctx context.Context, requestOptions ...RequestOption) (*billing.CatalogRevision, error) {
	if c.client.ownCatalog {
		return nil, fmt.Errorf("catalog revision requires merchant catalog authority")
	}
	var out billing.CatalogRevision
	if err := c.client.do(ctx, http.MethodGet, "/v1/merchant/catalog/revision", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
