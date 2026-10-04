package billing

import (
	"github.com/google/uuid"
)

// PaymentRecovery describes a payer-owned resource's current recovery state.
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
	ID     uuid.UUID `json:"id"`
	Status string    `json:"status"`
}

func (o PaymentOperation) Unresolved() bool {
	switch o.Status {
	case "pending", "in_flight", "failed_retryable", "unknown_needs_verify":
		return true
	}
	return false
}

type RetrySubscriptionNowRequest struct {
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
