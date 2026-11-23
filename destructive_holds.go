package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// ListDestructiveHolds lists incident episodes that originated at this merchant.
func (c *Client) ListDestructiveHolds(ctx context.Context, params billing.DestructiveHoldListParams) (*billing.ListPage[billing.DestructiveHold], error) {
	q := pageValues(nil, params.PageRequest)
	if err := setIDs(q, params.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.DestructiveHold]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/merchant/destructive-holds", q), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClearDestructiveHold clears an incident after review, recording who and
// why. It needs a person other than the one whose action opened it, holding
// root:admin-rate-limits:unlock and merchant configuration authority: over
// HTTP a self-acting human's own credential; in process the handle
// ActingFor names them, once the host has authenticated them.
func (c *Client) ClearDestructiveHold(ctx context.Context, id billing.DestructiveHoldID, req billing.ClearDestructiveHoldParams) (*billing.DestructiveHold, error) {
	if id.IsZero() {
		return nil, invalidErr("destructive hold id is required")
	}
	var out billing.DestructiveHold
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/destructive-holds/"+id.String()+"/clear", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
