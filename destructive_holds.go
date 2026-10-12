package openrails

import (
	"context"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/destructive"
	"net/http"
)

// ListDestructiveHolds lists incident episodes that originated at this merchant.
func (c *Client) ListDestructiveHolds(ctx context.Context, page billing.PageRequest, options ...RequestOption) (*billing.ListPage[billing.DestructiveHold], error) {
	var out billing.ListPage[billing.DestructiveHold]
	err := c.do(ctx, http.MethodGet, "/v1/merchant/destructive-holds?"+cursorQuery(page).Encode(), nil, &out, options...)
	return &out, err
}

// ClearDestructiveHold requires an interactive, self-acting operator credential
// with root:admin-rate-limits:unlock as well as merchant configuration authority.
// Embedded operators use Engine.ClearDestructiveHold instead of a host Client.
func (c *Client) ClearDestructiveHold(ctx context.Context, id billing.DestructiveHoldID, req billing.ClearDestructiveHoldParams, options ...RequestOption) (*billing.DestructiveHold, error) {
	if id.IsZero() {
		return nil, invalidErr("destructive hold id is required")
	}
	var out billing.DestructiveHold
	err := c.do(ctx, http.MethodPost, "/v1/merchant/destructive-holds/"+id.String()+"/clear", req, &out, options...)
	return &out, err
}

// ListOpenDestructiveHolds reads deployment-wide incidents for a trusted host
// operator. after is the last returned incident ID; zero begins the first page.
func (e *Engine) ListOpenDestructiveHolds(ctx context.Context, limit int, after billing.DestructiveHoldID) ([]billing.DestructiveHold, error) {
	var cursor *uuid.UUID
	if !after.IsZero() {
		id := after.UUID()
		cursor = &id
	}
	gate := e.engine.App.Runtime.DestructiveAdmissions
	addresses, err := gate.ListOpenHoldsDirectory(ctx, limit, cursor)
	if err != nil {
		return nil, err
	}
	out := make([]billing.DestructiveHold, 0, len(addresses))
	for _, address := range addresses {
		hold, err := gate.GetHold(ctx, address.MerchantID, address.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, hold.View())
	}
	return out, nil
}

// ClearDestructiveHold is a trusted in-process operator operation. The host
// authenticates the human before calling it and supplies that verified identity;
// never expose this method directly to unverified HTTP input.
func (e *Engine) ClearDestructiveHold(ctx context.Context, merchant billing.MerchantID, id billing.DestructiveHoldID, operator billing.DestructiveOperator, reason string) (*billing.DestructiveHold, error) {
	hold, err := e.engine.App.Runtime.DestructiveAdmissions.Clear(ctx, merchant.UUID(), id.UUID(), destructive.Actor{Issuer: operator.Issuer, ID: operator.ID, Credential: "trusted-operator"}, reason)
	if err != nil {
		return nil, err
	}
	view := hold.View()
	return &view, nil
}
