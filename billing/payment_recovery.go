package billing

import (
	"github.com/google/uuid"
)

// PaymentRecovery describes a customer-owned resource's current recovery state.
// It is a view, not permission to charge: acceptance checks the same facts
// again under the resource lock.
type PaymentRecovery struct {
	// LastFailureReason is the normalized machine category of the latest applicable
	// failure, never raw provider text. It is absent for successful recovery, old
	// subscription periods, or a newer pending attempt.
	LastFailureReason string            `json:"last_failure_reason,omitempty"`
	Retryable         bool              `json:"retryable"`
	BlockedReason     string            `json:"blocked_reason,omitempty"`
	Operation         *PaymentOperation `json:"operation,omitempty"`
}

type PaymentOperation struct {
	ID     PaymentOperationID `json:"id"`
	Status string             `json:"status"`
}

// PaymentOperationID names one payment operation; on the wire "pop_<uuid>".
type PaymentOperationID uuid.UUID

const paymentOperationIDPrefix = "pop_"

func ParsePaymentOperationID(s string) (PaymentOperationID, error) {
	u, err := parsePrefixedID("payment operation", paymentOperationIDPrefix, s)
	return PaymentOperationID(u), err
}

func (id PaymentOperationID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id PaymentOperationID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id PaymentOperationID) String() string {
	return formatPrefixedID(paymentOperationIDPrefix, uuid.UUID(id))
}
func (id PaymentOperationID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *PaymentOperationID) UnmarshalText(b []byte) error {
	v, err := ParsePaymentOperationID(string(b))
	*id = v
	return err
}

func (o PaymentOperation) Unresolved() bool {
	switch o.Status {
	case "pending", "in_flight", "failed_retryable", "unknown_needs_verify":
		return true
	}
	return false
}

type RetrySubscriptionNowParams struct {
	SubscriptionID SubscriptionID `json:"-"`
	IdempotencyKey string         `json:"-"`
	// If supplied, the method must be the subscription's current saved method.
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
}

type SubscriptionRetryNowResult struct {
	Subscription Subscription     `json:"subscription"`
	Operation    PaymentOperation `json:"operation"`
	Replayed     bool             `json:"replayed"`
}
