package openrails

import (
	"context"
	"net/http"
	"strconv"

	"github.com/open-rails/openrails/billing"
)

// ListCustomerNotifications reads the customer's retained billing inbox,
// newest first. It requires merchant:customers:read. Hosts must authenticate
// their user and map it to this merchant's customer before exposing the result.
//
// This is an inbox snapshot, not an acknowledged event stream: retention and
// supersession can remove rows. Importers rescan retained pages and deduplicate
// by (source installation, merchant_id, id), keeping their own read state.
func (c *Client) ListCustomerNotifications(ctx context.Context, customer billing.CustomerID, params billing.NotificationListParams, options ...RequestOption) (*billing.ListPage[billing.Notification], error) {
	if customer.IsZero() {
		return nil, invalidErr("customer_id is required")
	}
	q := cursorQuery(params.PageRequest)
	if params.Seen != nil {
		q.Set("seen", strconv.FormatBool(*params.Seen))
	}
	var out billing.ListPage[billing.Notification]
	path := withQuery("/v1/app/customers/"+customer.String()+"/notifications", q)
	if err := c.do(ctx, http.MethodGet, path, nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
