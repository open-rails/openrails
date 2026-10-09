package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// GetAdminAccess reports what the caller may use of each staff route group
// at the merchant. In process, the Client is the merchant's owner and holds
// every group.
func (c *Client) GetAdminAccess(ctx context.Context, options ...RequestOption) (*billing.AdminAccess, error) {
	var out billing.AdminAccess
	if err := c.do(ctx, http.MethodGet, "/v1/admin/access", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
