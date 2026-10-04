package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// ListHostEvents pages the merchant's host events, oldest first. Process each
// idempotently, AcknowledgeHostEvent it, and list again.
func (c *Client) ListHostEvents(ctx context.Context, req billing.ListHostEventsRequest, options ...RequestOption) (billing.ListPage[billing.HostEvent], error) {
	q := cursorQuery(req.PageRequest)
	if req.Type != "" {
		q.Set("type", string(req.Type))
	}
	if req.IncludeAcknowledged {
		q.Set("include_acknowledged", "true")
	}
	if !req.PaymentID.IsZero() {
		q.Set("payment_id", req.PaymentID.String())
	}
	var out billing.ListPage[billing.HostEvent]
	err := c.do(ctx, http.MethodGet, "/v1/merchant/host-events?"+q.Encode(), nil, &out, options...)
	return out, err
}

// AcknowledgeHostEvent is idempotent. Call only after the host's idempotent
// processing has committed; an unacknowledged event is redelivered.
func (c *Client) AcknowledgeHostEvent(ctx context.Context, id billing.HostEventID, options ...RequestOption) (*billing.HostEvent, error) {
	if id.IsZero() {
		return nil, invalidErr("host event id is required")
	}
	var out billing.HostEvent
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/host-events/"+id.String()+"/acknowledge", nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
