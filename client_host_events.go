package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

// ListHostEvents returns the merchant's host events, oldest first:
// unacknowledged ones unless options asks for all. Process each idempotently,
// AcknowledgeHostEvent it, and list again.
func (c *Client) ListHostEvents(ctx context.Context, options billing.HostEventListOptions, requestOptions ...RequestOption) ([]billing.HostEvent, error) {
	query := url.Values{}
	if options.Type != "" {
		query.Set("type", string(options.Type))
	}
	if options.Limit != 0 {
		query.Set("limit", strconv.Itoa(options.Limit))
	}
	if options.IncludeAcknowledged {
		query.Set("include_acknowledged", "true")
	}
	if !options.PaymentID.IsZero() {
		query.Set("payment_id", options.PaymentID.String())
	}
	var out []billing.HostEvent
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/host-events?"+query.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out, nil
}

// AcknowledgeHostEvent is idempotent. Call only after the host's idempotent
// processing has committed; an unacknowledged event is redelivered.
func (c *Client) AcknowledgeHostEvent(ctx context.Context, id uuid.UUID, requestOptions ...RequestOption) error {
	event, err := requireUUID("event_id", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/v1/merchant/host-events/"+event+"/acknowledge", nil, nil, requestOptions...)
}
