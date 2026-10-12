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
	// HostEventProductEntitlementsChanged is a product gaining or losing keys:
	// every holder's access changed with it.
	HostEventProductEntitlementsChanged HostEventType = "product.entitlements_changed"
	// Order transitions: fulfil on order.completed.
	HostEventOrderCompleted      HostEventType = "order.completed"
	HostEventOrderRequiresAction HostEventType = "order.requires_action"
	HostEventOrderPaymentFailed  HostEventType = "order.payment_failed"
	HostEventOrderCanceled       HostEventType = "order.canceled"
	HostEventOrderExpired        HostEventType = "order.expired"
	// HostEventOrderVoided: a renewal order its subscription's end left
	// unowed.
	HostEventOrderVoided HostEventType = "order.voided"
	// A customer linked to, or unlinked from, an openrails.dev account.
	HostEventCustomerNetworkLinked   HostEventType = "customer.network_linked"
	HostEventCustomerNetworkUnlinked HostEventType = "customer.network_unlinked"
	// HostEventMandateRevoked: the customer revoked one of its charge
	// agreements; the merchant charges nothing more under it.
	HostEventMandateRevoked HostEventType = "mandate.revoked"
	// A watched customer's balance (every prepaid one, and every one with a
	// running charge) went under its low line, or its capacity reached zero:
	// once per crossing, re-armed by a funding.
	HostEventBalanceLow      HostEventType = "balance.low"
	HostEventBalanceDepleted HostEventType = "balance.depleted"
)

// BalanceHostEvent is balance.low or balance.depleted: the customer's balance
// in one currency as the crossing was seen, at AsOf. LowBalanceAmount is the
// effective line it was judged at: the larger of the amount line and the burn
// rate (AccrualRatePerHour + UsageRatePerHour) times the runway line.
// DepletesAt is exact (running charges and credit lot lapses), null when
// neither moves the balance; RunwayEndsAt is when capacity reaches zero at the
// burn rate, null at rate zero. On balance.depleted, stop what the customer's
// running charges pay for.
type BalanceHostEvent struct {
	CustomerID         CustomerID `json:"customer_id"`
	Currency           string     `json:"currency"`
	AvailableAmount    int64      `json:"available_amount,string"`
	OwedAmount         int64      `json:"owed_amount,string"`
	AccrualRatePerHour int64      `json:"accrual_rate_per_hour,string"`
	UsageRatePerHour   int64      `json:"usage_rate_per_hour,string"`
	LowBalanceAmount   int64      `json:"low_balance_amount,string"`
	AsOf               time.Time  `json:"as_of"`
	DepletesAt         *time.Time `json:"depletes_at"`
	RunwayEndsAt       *time.Time `json:"runway_ends_at"`
}

// OrderHostEvent is one order transition: the order's status and its
// payment's as the transition left them. Number and PaymentID are set on
// order.completed.
type OrderHostEvent struct {
	OrderID       OrderID            `json:"order_id"`
	CustomerID    CustomerID         `json:"customer_id"`
	Status        OrderStatus        `json:"status"`
	PaymentStatus OrderPaymentStatus `json:"payment_status"`
	Total         int64              `json:"total,string"`
	Currency      string             `json:"currency"`
	Number        *string            `json:"number"`
	PaymentID     *PaymentID         `json:"payment_id"`
}

// ProductEntitlementsChangedEvent is one key edit of a product. Holders is how
// many customers held the product when it changed.
type ProductEntitlementsChangedEvent struct {
	ProductID  ProductID `json:"product_id"`
	ProductKey string    `json:"product_key"`
	Added      []string  `json:"added"`
	Removed    []string  `json:"removed"`
	Holders    int64     `json:"holders"`
}

// PaymentSettledEvent is one successful rail payment. CustomerID and what it
// paid (an order, a subscription's period, or a one-price sale's price) come
// from the authoritative payment row so a host can route the settlement
// without a second read.
type PaymentSettledEvent struct {
	PaymentID      PaymentID       `json:"payment_id"`
	CustomerID     CustomerID      `json:"customer_id"`
	OrderID        *OrderID        `json:"order_id,omitempty"`
	PriceID        *PriceID        `json:"price_id,omitempty"`
	SubscriptionID *SubscriptionID `json:"subscription_id,omitempty"`
	Amount         int64           `json:"amount,string"`
	Currency       string          `json:"currency"`
}

type DelinquencyHostEvent struct {
	CustomerID       CustomerID `json:"customer_id"`
	Currency         string     `json:"currency"`
	FromState        string     `json:"from_state"`
	ToState          string     `json:"to_state"`
	OverdueStartedAt *time.Time `json:"overdue_started_at,omitempty"`
	OverdueAmount    int64      `json:"overdue_amount,string"`
	OverdueOrders    int64      `json:"overdue_orders"`
	GraceDays        int64      `json:"grace_days"`
	AmountFloor      int64      `json:"amount_floor,string"`
}

// HostEvent has exactly one payload, selected by Type. Acknowledgment is a
// durable consumer action; it never changes a customer or merchant notification.
type HostEvent struct {
	ID             HostEventID           `json:"id"`
	MerchantID     MerchantID            `json:"merchant_id"`
	Type           HostEventType         `json:"type"`
	OccurredAt     time.Time             `json:"occurred_at"`
	AcknowledgedAt *time.Time            `json:"acknowledged_at"`
	Payment        *PaymentSettledEvent  `json:"payment"`
	Delinquency    *DelinquencyHostEvent `json:"delinquency"`
	// ProductEntitlements is set for product.entitlements_changed.
	ProductEntitlements *ProductEntitlementsChangedEvent `json:"product_entitlements"`
	// Order is set for the order.* events.
	Order *OrderHostEvent `json:"order"`
	// CustomerNetwork is set for the customer.network_* events.
	CustomerNetwork *CustomerNetworkHostEvent `json:"customer_network"`
	// Mandate is set for mandate.revoked.
	Mandate *MandateHostEvent `json:"mandate"`
	// Balance is set for balance.low and balance.depleted.
	Balance *BalanceHostEvent `json:"balance"`
}

// AcknowledgeHostEventsParams names 1 to MaxBatchItems host events to
// acknowledge.
type AcknowledgeHostEventsParams struct {
	HostEventIDs []HostEventID `json:"host_event_ids"`
}

// HostEventLookup answers every requested host event; one that does not exist
// is null.
type HostEventLookup struct {
	HostEvents map[HostEventID]*HostEvent `json:"host_events"`
}

// HostEventListParams pages the merchant's host events, oldest first:
// unacknowledged ones unless IncludeAcknowledged. A consumer can select its
// event type so unrelated pending events cannot starve its work. Acknowledge
// processed events and list again from the start; a cursor is for reading
// history, never a high-water mark.
//
// IDs instead reads 1 to MaxBatchItems named events in one page, acknowledged
// or not; unknown ones are absent.
type HostEventListParams struct {
	PageRequest
	IDs                 []HostEventID
	Type                HostEventType
	IncludeAcknowledged bool
	PaymentID           PaymentID
}
