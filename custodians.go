package openrails

import (
	"context"
	"net/http"
	"net/url"

	"github.com/open-rails/openrails/billing"
)

// ListCustodians reads the merchant's declared vault accounts without secrets.
func (c *Client) ListCustodians(ctx context.Context, requestOptions ...RequestOption) (*billing.ListPage[billing.Custodian], error) {
	var out billing.ListPage[billing.Custodian]
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/custodians", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// PutCustodian configures one merchant vault at its explicit revision.
func (c *Client) PutCustodian(ctx context.Context, key string, params billing.PutCustodianParams, requestOptions ...RequestOption) (*billing.Custodian, error) {
	if key == "" {
		return nil, invalidErr("custodian key is required")
	}
	var out billing.Custodian
	if err := c.do(ctx, http.MethodPut, "/v1/merchant/custodians/"+url.PathEscape(key), params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
