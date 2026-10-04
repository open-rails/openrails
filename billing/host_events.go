package billing

import (
	"time"

	"github.com/google/uuid"
)

// HostEventID names one host event; on the wire "hev_<uuid>".
type HostEventID uuid.UUID

const HostEventIDPrefix = "hev_"

func ParseHostEventID(s string) (HostEventID, error) {
	u, err := parsePrefixedID("host event", HostEventIDPrefix, s)
	return HostEventID(u), err
}

func (id HostEventID) UUID() uuid.UUID              { return uuid.UUID(id) }
func (id HostEventID) IsZero() bool                 { return uuid.UUID(id) == uuid.Nil }
func (id HostEventID) String() string               { return formatPrefixedID(HostEventIDPrefix, uuid.UUID(id)) }
func (id HostEventID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *HostEventID) UnmarshalText(b []byte) error {
	v, err := ParseHostEventID(string(b))
	*id = v
	return err
}

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
	ID             HostEventID           `json:"id"`
	MerchantID     MerchantID            `json:"merchant_id"`
	Type           HostEventType         `json:"type"`
	OccurredAt     time.Time             `json:"occurred_at"`
	AcknowledgedAt *time.Time            `json:"acknowledged_at"`
	Payment        *PaymentSettledEvent  `json:"payment"`
	Delinquency    *DelinquencyHostEvent `json:"delinquency"`
}

// ListHostEventsRequest pages the merchant's host events, oldest first:
// unacknowledged ones unless IncludeAcknowledged. A consumer can select its
// event type so unrelated pending events cannot starve its work. Acknowledge
// processed events and list again from the start; a cursor is for reading
// history, never a high-water mark.
type ListHostEventsRequest struct {
	PageRequest
	Type                HostEventType
	IncludeAcknowledged bool
	PaymentID           PaymentID
}
