package openrails

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// ListHostEvents pages the merchant's host events, oldest first. Process each
// idempotently, AcknowledgeHostEvents it, and list again.
func (c *Client) ListHostEvents(ctx context.Context, req billing.HostEventListParams, options ...RequestOption) (*billing.ListPage[billing.HostEvent], error) {
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
	if err := setIDs(q, req.IDs); err != nil {
		return nil, err
	}
	var out billing.ListPage[billing.HostEvent]
	if err := c.do(ctx, http.MethodGet, "/v1/app/host-events?"+q.Encode(), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}

// AcknowledgeHostEvents acknowledges 1 to billing.MaxBatchItems host events.
// It is idempotent. Call only after the host's idempotent processing of them
// has committed; an unacknowledged event is redelivered. Every requested event
// is in the answer; one that does not exist is nil.
func (c *Client) AcknowledgeHostEvents(ctx context.Context, ids []billing.HostEventID, options ...RequestOption) (map[billing.HostEventID]*billing.HostEvent, error) {
	if err := batchIDs("host_event_ids", ids, billing.MaxBatchItems); err != nil {
		return nil, err
	}
	var out billing.HostEventLookup
	if err := c.do(ctx, http.MethodPost, "/v1/app/host-events/acknowledge", billing.AcknowledgeHostEventsParams{HostEventIDs: ids}, &out, options...); err != nil {
		return nil, err
	}
	return out.HostEvents, nil
}
