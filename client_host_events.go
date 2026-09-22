package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// HostEventType names the durable event's typed payload.
type HostEventType string

const (
	HostEventPaymentSettled     HostEventType = "payment.settled"
	HostEventDelinquencyGrace   HostEventType = "delinquency.grace"
	HostEventDelinquencyEntered HostEventType = "delinquency.entered"
	HostEventDelinquencyCleared HostEventType = "delinquency.cleared"
)

// PaymentSettledEvent is one successful rail payment. CustomerID and PriceID
// come from the authoritative payment row so a host can route the settlement
// without a second read; SubscriptionID is set for renewal payments.
type PaymentSettledEvent struct {
	PaymentID      PaymentID       `json:"payment_id"`
	CustomerID     string          `json:"customer_id"`
	PriceID        string          `json:"price_id"`
	SubscriptionID *SubscriptionID `json:"subscription_id,omitempty"`
	Amount         int64           `json:"amount,string"`
	Currency       string          `json:"currency"`
}

type DelinquencyHostEvent struct {
	CustomerID      string     `json:"customer_id"`
	Currency        string     `json:"currency"`
	FromState       string     `json:"from_state"`
	ToState         string     `json:"to_state"`
	OverdueSince    *time.Time `json:"overdue_since,omitempty"`
	OverdueAmount   int64      `json:"overdue_amount,string"`
	OverdueInvoices int64      `json:"overdue_invoices"`
	GraceDays       int64      `json:"grace_days"`
	AmountFloor     int64      `json:"amount_floor,string"`
}

// HostEvent has exactly one payload, selected by Type. Acknowledgment is a
// durable consumer action; it never changes a payer or merchant notification.
type HostEvent struct {
	ID             uuid.UUID             `json:"id"`
	MerchantID     MerchantID            `json:"merchant_id"`
	Type           HostEventType         `json:"type"`
	OccurredAt     time.Time             `json:"occurred_at"`
	AcknowledgedAt *time.Time            `json:"acknowledged_at,omitempty"`
	Payment        *PaymentSettledEvent  `json:"payment,omitempty"`
	Delinquency    *DelinquencyHostEvent `json:"delinquency,omitempty"`
}

const MaxHostEventPageSize = 1000

// HostEventListOptions filters the bounded stream. By default only unacknowledged
// events are returned, oldest first. A consumer can select its event type so
// unrelated pending events cannot starve its work. Acknowledge processed events
// and fetch again; persisting a UUID high-water mark could skip late commits.
type HostEventListOptions struct {
	Type                HostEventType
	Limit               int
	IncludeAcknowledged bool
	PaymentID           PaymentID
}

func (c *Client) ListHostEvents(ctx context.Context, options HostEventListOptions) ([]HostEvent, error) {
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
	var out []HostEvent
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/host-events?"+query.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AcknowledgeHostEvent is idempotent. Call only after the host's idempotent
// processing has committed; an unacknowledged event is redelivered.
func (c *Client) AcknowledgeHostEvent(ctx context.Context, id uuid.UUID) error {
	event, err := requireUUID("event_id", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/v1/merchant/host-events/"+event+"/acknowledge", nil, nil)
}
