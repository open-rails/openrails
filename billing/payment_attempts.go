package billing

import (
	"time"
)

// PaymentAttempt is one authorization a PSP answered (#1110): a card
// verification, a sale, a rebill or a retry, approved or not. Category,
// Reason and Action are the one decline classifier's; the codes and texts
// are the PSP's verbatim. Amount is native units ("0" for a verification).
type PaymentAttempt struct {
	ID PaymentAttemptID `json:"id"`
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
	Reason       DeclineReason `json:"reason"`
	Action       string        `json:"action"`
	ResponseCode string        `json:"response_code"`
	ResponseText string        `json:"response_text"`
	// IssuerCode and IssuerText are the issuer's raw answer, once enriched.
	IssuerCode string `json:"issuer_code"`
	IssuerText string `json:"issuer_text"`
	AVSResult  string `json:"avs_result"`
	CVVResult  string `json:"cvv_result"`
	// Card is the card the PSP answered for; CardBIN its leading digits.
	Card          *CardDetails `json:"card"`
	CardBIN       string       `json:"card_bin"`
	TokenType     string       `json:"token_type"`
	TransactionID string       `json:"transaction_id"`
	Rail          string       `json:"rail"`
	PSPID         PSPID        `json:"psp_id"`
	CustomerID    CustomerID   `json:"customer_id"`
	Amount        int64        `json:"amount,string"`
	Currency      string       `json:"currency"`
	AttemptedAt   time.Time    `json:"attempted_at"`
	// CheckoutID groups one buyer's attempts on one target (a price or
	// card_save, CheckoutTarget) until it is approved.
	CheckoutID      string           `json:"checkout_id"`
	CheckoutTarget  string           `json:"checkout_target"`
	CycleID         *RebillCycleID   `json:"cycle_id"`
	SubscriptionID  *SubscriptionID  `json:"subscription_id"`
	PaymentMethodID *PaymentMethodID `json:"payment_method_id"`
	PaymentID       *PaymentID       `json:"payment_id"`
	EnrichedAt      *time.Time       `json:"enriched_at"`
	// MandateID is the mandate whose references the attempt sent, and
	// SentInitialTransactionID the initial transaction id it sent verbatim
	// (empty where the provider links its own, as Stripe does).
	MandateID                *MandateID `json:"mandate_id"`
	SentInitialTransactionID string     `json:"sent_initial_transaction_id"`
}

// PaymentAttemptListParams selects attempts, newest first; every field is
// optional, and a list matches any of its values. Since and Until bound
// attempted_at to [Since, Until).
//
// IDs instead reads 1 to MaxBatchItems named attempts in one page; unknown
// ones are absent.
type PaymentAttemptListParams struct {
	PageRequest
	IDs                                                    []PaymentAttemptID
	Kind, Owner, Category, Reason, ResponseCode, CardEntry []string
	Source, ObservedVia, AVSResult, CVVResult              []string
	PSPID                                                  PSPID
	CustomerID                                             CustomerID
	// CheckoutID is the grouping id of one buyer's attempts on one target.
	CheckoutID     string
	SubscriptionID SubscriptionID
	CycleID        RebillCycleID
	Since, Until   time.Time
}

// RebillCycle is one paid period that came due (#1111) and what its attempts
// decided. Outcome is collected, lost (closed without a collection) or open;
// a cycle closes when collected, when its subscription is canceled, or 15
// days past due.
type RebillCycle struct {
	ID             RebillCycleID  `json:"id"`
	SubscriptionID SubscriptionID `json:"subscription_id"`
	CustomerID     CustomerID     `json:"customer_id"`
	PSPID          PSPID          `json:"psp_id"`
	Rail           string         `json:"rail"`
	Owner          string         `json:"owner"`
	DueAt          time.Time      `json:"due_at"`
	Amount         int64          `json:"amount,string"`
	Currency       string         `json:"currency"`
	// FirstOutcome is approved, declined, error, missed or pending.
	FirstOutcome string     `json:"first_outcome"`
	Outcome      string     `json:"outcome"`
	MissedAt     *time.Time `json:"missed_at"`
	MissReason   string     `json:"miss_reason"`
	CollectedAt  *time.Time `json:"collected_at"`
	// RecoveredBy is what collected a cycle whose first outcome failed:
	// dunning_retry, customer_retry, updated_card or late_provider_charge.
	RecoveredBy string    `json:"recovered_by"`
	ClosesAt    time.Time `json:"closes_at"`
	// Attempts are the cycle's attempts, oldest first: read with a single
	// cycle, null in lists.
	Attempts []PaymentAttempt `json:"attempts"`
}

// RebillCycleListParams selects cycles, latest due first; every field is
// optional, and a list matches any of its values. DueSince and DueUntil bound
// due_at to [DueSince, DueUntil).
//
// IDs instead reads 1 to MaxBatchItems named cycles in one page; unknown ones
// are absent.
type RebillCycleListParams struct {
	PageRequest
	IDs                                      []RebillCycleID
	Owner, FirstOutcome, MissReason, Outcome []string
	PSPID                                    PSPID
	SubscriptionID                           SubscriptionID
	DueSince, DueUntil                       time.Time
}
