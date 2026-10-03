package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// EnsureCustomer materializes the customer record for id under the bound
// merchant, or refreshes last_seen_at when it already exists. Commerce writes
// materialize customers on demand; call this when a host must reference the
// customer before its first purchase.
func (c *Client) EnsureCustomer(ctx context.Context, id string, requestOptions ...RequestOption) (*billing.Customer, error) {
	path, err := customerPath(id)
	if err != nil {
		return nil, err
	}
	var out billing.Customer
	if err := c.do(ctx, http.MethodPut, path, struct{}{}, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
