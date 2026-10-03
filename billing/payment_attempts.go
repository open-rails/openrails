package billing

import (
	"time"
)

// PaymentAttempt is one authorization a PSP answered (#1110): a card
// verification, a sale, a rebill or a retry, approved or not. Category,
// Reason and Action are the one decline classifier's; the codes and texts
// are the PSP's verbatim. Amount is native units ("0" for a verification).
type PaymentAttempt struct {
	ID     PaymentAttemptID `json:"id"`
	Object string           `json:"object"`
	// Kind is verify, initial, upgrade, rebill, dunning_retry, customer_retry
	// or invoice.
	Kind string `json:"kind"`
	// Owner is who collects the subscription: engine, nmi_schedule, provider
	// or none.
	Owner string `json:"owner"`
	// CardEntry is new (entered in this checkout or card add) or saved.
	CardEntry string `json:"card_entry"`
	// Source is openrails, provider_schedule or external; ObservedVia is how
	// OpenRails learned the answer: response, webhook or pull.
	Source       string        `json:"source"`
	ObservedVia  string        `json:"observed_via"`
	Category     string        `json:"category"`
	Reason       DeclineReason `json:"reason,omitempty"`
	Action       string        `json:"action,omitempty"`
	ResponseCode string        `json:"response_code,omitempty"`
	ResponseText string        `json:"response_text,omitempty"`
	// IssuerCode and IssuerText are the issuer's raw answer, once enriched.
	IssuerCode    string `json:"issuer_code,omitempty"`
	IssuerText    string `json:"issuer_text,omitempty"`
	AVSResult     string `json:"avs_result,omitempty"`
	CVVResult     string `json:"cvv_result,omitempty"`
	CardBrand     string `json:"card_brand,omitempty"`
	CardLast4     string `json:"card_last4,omitempty"`
	CardBIN       string `json:"card_bin,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	TransactionID string `json:"transaction_id,omitempty"`
	Rail          string `json:"rail"`
	// PSPID is the provider account's plain UUID.
	PSPID       string    `json:"psp_id"`
	CustomerID  string    `json:"customer_id"`
	Amount      int64     `json:"amount,string"`
	Currency    string    `json:"currency,omitempty"`
	AttemptedAt time.Time `json:"attempted_at"`
	// CheckoutID groups one buyer's attempts on one target (a price or
	// card_save, CheckoutTarget) until it is approved.
	CheckoutID      string           `json:"checkout_id,omitempty"`
	CheckoutTarget  string           `json:"checkout_target,omitempty"`
	CycleID         *RebillCycleID   `json:"cycle_id,omitempty"`
	SubscriptionID  *SubscriptionID  `json:"subscription_id,omitempty"`
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
	PaymentID       *PaymentID       `json:"payment_id,omitempty"`
	EnrichedAt      *time.Time       `json:"enriched_at,omitempty"`
}

// PaymentAttemptFilter selects attempts, newest first; every field is
// optional, and a list matches any of its values. Since and Until bound
// attempted_at to [Since, Until).
type PaymentAttemptFilter struct {
	PageOptions
	Kind, Owner, Category, Reason, ResponseCode, CardEntry []string
	Source, ObservedVia, AVSResult, CVVResult              []string
	// PSPID, CustomerID and CheckoutID are plain UUIDs.
	PSPID, CustomerID, CheckoutID string
	SubscriptionID                SubscriptionID
	CycleID                       RebillCycleID
	Since, Until                  time.Time
}

// RebillCycle is one paid period that came due (#1111) and what its attempts
// decided. Outcome is collected, lost (closed without a collection) or open;
// a cycle closes when collected, when its subscription is cancelled, or 15
// days past due.
type RebillCycle struct {
	ID             RebillCycleID  `json:"id"`
	Object         string         `json:"object"`
	SubscriptionID SubscriptionID `json:"subscription_id"`
	CustomerID     string         `json:"customer_id"`
	PSPID          string         `json:"psp_id"`
	Rail           string         `json:"rail"`
	Owner          string         `json:"owner"`
	DueAt          time.Time      `json:"due_at"`
	Amount         int64          `json:"amount,string"`
	Currency       string         `json:"currency"`
	// FirstOutcome is approved, declined, error, missed or pending.
	FirstOutcome string     `json:"first_outcome"`
	Outcome      string     `json:"outcome"`
	MissedAt     *time.Time `json:"missed_at,omitempty"`
	MissReason   string     `json:"miss_reason,omitempty"`
	CollectedAt  *time.Time `json:"collected_at,omitempty"`
	// RecoveredBy is what collected a cycle whose first outcome failed:
	// dunning_retry, customer_retry, updated_card or late_provider_charge.
	RecoveredBy string    `json:"recovered_by,omitempty"`
	ClosesAt    time.Time `json:"closes_at"`
	// Attempts are the cycle's attempts, oldest first (GetRebillCycle only).
	Attempts []PaymentAttempt `json:"attempts,omitempty"`
}

// RebillCycleFilter selects cycles, latest due first; every field is
// optional, and a list matches any of its values. DueSince and DueUntil bound
// due_at to [DueSince, DueUntil).
type RebillCycleFilter struct {
	PageOptions
	Owner, FirstOutcome, MissReason, Outcome []string
	// PSPID is the provider account's plain UUID.
	PSPID              string
	SubscriptionID     SubscriptionID
	DueSince, DueUntil time.Time
}
