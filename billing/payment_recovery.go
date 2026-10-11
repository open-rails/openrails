package billing

// PaymentRecovery describes a customer-owned resource's current recovery state.
// It is a view, not permission to charge: acceptance checks the same facts
// again under the resource lock.
type PaymentRecovery struct {
	// LastFailureReason is the normalized machine category of the latest applicable
	// failure, never raw provider text. It is absent for successful recovery, old
	// subscription periods, or a newer pending attempt.
	LastFailureReason string `json:"last_failure_reason,omitempty"`
	Retryable         bool   `json:"retryable"`
	BlockedReason     string `json:"blocked_reason,omitempty"`
	// OrderID is an engine-owned subscription's unpaid renewal: the order its
	// customer pays (POST /v1/me/orders/{id}/pay) or confirms.
	OrderID *OrderID `json:"order_id,omitempty"`
}

type RetrySubscriptionNowParams struct {
	SubscriptionID SubscriptionID `json:"-"`
	IdempotencyKey string         `json:"-"`
	// If supplied, the method must be the subscription's current saved method.
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
}

// SubscriptionRetryNowResult is a retried renewal. Status is succeeded, or
// processing (HTTP 202) while the provider's outcome is unresolved; the same
// Idempotency-Key reads it back. A decline answers 402.
type SubscriptionRetryNowResult struct {
	Subscription Subscription `json:"subscription"`
	Status       string       `json:"status"`
	Replayed     bool         `json:"replayed"`
}
