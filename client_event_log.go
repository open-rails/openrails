package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/open-rails/openrails/billing"
)

// ListEvents reads retained facts of the handle's merchant. Reading starts no
// worker and changes no delivery state. Persist NextCursor after processing.
func (c *Client) ListEvents(ctx context.Context, query billing.EventQuery, options ...RequestOption) (*billing.EventPage, error) {
	values := url.Values{}
	if query.After != "" {
		values.Set("after", query.After)
	}
	if query.Limit != 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	for _, kind := range query.Types {
		values.Add("type", kind)
	}
	var out billing.EventPage
	if err := c.do(ctx, http.MethodGet, "/v1/app/events?"+values.Encode(), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
