package openrails

import (
	"context"
	"net/http"

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
