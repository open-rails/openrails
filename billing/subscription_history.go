package billing

import "time"

// SubscriptionHistory is retained evidence about one subscription, newest
// first. It does not predict the next charge or replace current recovery state.
type SubscriptionHistory struct {
	Events   ListPage[SubscriptionHistoryEntry] `json:"events"`
	Coverage SubscriptionHistoryCoverage        `json:"coverage"`
}

// SubscriptionHistoryCoverage describes what this reader can establish.
// Missing history is not evidence that an action never happened. RetentionDays
// is the pruning policy, not a guarantee of continuous coverage: imports,
// archival and late observations can leave gaps.
type SubscriptionHistoryCoverage struct {
	Sources       []string `json:"sources"`
	RetentionDays int      `json:"retention_days"`
	Gaps          []string `json:"gaps"`
}

// SubscriptionHistoryEntry contains exactly one source fact. ID is an opaque,
// source-qualified identifier, stable across reads. At is the source's recorded
// time: database observation time for transitions/operations, provider attempt
// time for payment attempts. RecordedAt is when OpenRails stored the fact.
// Neither implies the original signup time of an imported subscription.
type SubscriptionHistoryEntry struct {
	ID         string                         `json:"id"`
	At         time.Time                      `json:"at"`
	RecordedAt time.Time                      `json:"recorded_at"`
	Kind       string                         `json:"kind"`
	Transition *SubscriptionHistoryTransition `json:"transition,omitempty"`
	Attempt    *SubscriptionHistoryAttempt    `json:"attempt,omitempty"`
	Operation  *SubscriptionHistoryOperation  `json:"operation,omitempty"`
}

// SubscriptionHistoryTransition is the state actually committed. Created
// means first recorded here; an imported subscription may have begun earlier.
type SubscriptionHistoryTransition struct {
	FromStatus    *SubscriptionStatus `json:"from_status"`
	ToStatus      SubscriptionStatus  `json:"to_status"`
	Decision      string              `json:"decision,omitempty"`
	CancelType    string              `json:"cancel_type,omitempty"`
	FromPeriodEnd *time.Time          `json:"from_period_end"`
	ToPeriodEnd   *time.Time          `json:"to_period_end"`
}

// SubscriptionHistoryAttempt is a provider answer, not an execution promise.
// Category and Reason retain the existing decline classification. Raw provider
// messages, card data and stored-credential references are deliberately absent.
// Action is the answer's classification, not a promise to execute a retry.
type SubscriptionHistoryAttempt struct {
	ID              PaymentAttemptID `json:"id"`
	Kind            string           `json:"kind"`
	Category        string           `json:"category"`
	Reason          string           `json:"reason,omitempty"`
	Action          string           `json:"action,omitempty"`
	Source          string           `json:"source"`
	ObservedVia     string           `json:"observed_via"`
	Amount          int64            `json:"amount,string"`
	Currency        string           `json:"currency"`
	PSPID           PSPID            `json:"psp_id"`
	OrderID         *OrderID         `json:"order_id,omitempty"`
	PaymentID       *PaymentID       `json:"payment_id,omitempty"`
	RenewalID       *RenewalID       `json:"renewal_id,omitempty"`
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
	OperationID     string           `json:"operation_id,omitempty"`
}

// SubscriptionHistoryOperation is a recorded provider execution phase. An
// unknown outcome is not a decline, parked means execution was held,
// and succeeded refers to this operation, not necessarily a payment.
type SubscriptionHistoryOperation struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Phase   string `json:"phase"`
	Attempt int    `json:"attempt"`
}
