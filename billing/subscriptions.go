package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// SubscriptionStatus is a subscription's lifecycle state: whether it will
// rebill.
type SubscriptionStatus string

const (
	SubscriptionPending        SubscriptionStatus = "pending"
	SubscriptionActive         SubscriptionStatus = "active"
	SubscriptionPastDue        SubscriptionStatus = "past_due"
	SubscriptionAwaitingMethod SubscriptionStatus = "awaiting_method"
	SubscriptionCanceled       SubscriptionStatus = "canceled"
	// SubscriptionUnverified: the provider must say whether it still bills.
	SubscriptionUnverified SubscriptionStatus = "unverified"
)

// SubscriptionSort orders a subscription list. Each order has one direction;
// ordered by a time a subscription may lack, the list holds only those with it.
type SubscriptionSort string

const (
	// SubscriptionSortCreatedAt is the newest first, the default.
	SubscriptionSortCreatedAt SubscriptionSort = "created_at"
	// SubscriptionSortCurrentPeriodEndsAt is the soonest period end first.
	SubscriptionSortCurrentPeriodEndsAt SubscriptionSort = "current_period_ends_at"
	// SubscriptionSortCanceledAt is the latest cancellation first.
	SubscriptionSortCanceledAt SubscriptionSort = "canceled_at"
)

// SubscriptionSorts is every SubscriptionSort.
func SubscriptionSorts() []SubscriptionSort {
	return []SubscriptionSort{SubscriptionSortCreatedAt, SubscriptionSortCurrentPeriodEndsAt, SubscriptionSortCanceledAt}
}

// SubscriptionListParams filters one page of the merchant's subscriptions,
// in Sort's order (newest first by default).
type SubscriptionListParams struct {
	PageRequest
	// IDs instead reads 1 to MaxBatchItems named subscriptions in one page,
	// whatever their state; unknown ones are absent.
	IDs        []SubscriptionID   `form:"ids,comma"`
	CustomerID CustomerID         `form:"customer_id"`
	Status     SubscriptionStatus `form:"status"`
	// Dunning keeps the subscriptions in dunning: past_due or
	// awaiting_method, each with its Dunning.
	Dunning bool             `form:"dunning"`
	Rail    Rail             `form:"rail"`
	PriceID PriceID          `form:"price_id"`
	Sort    SubscriptionSort `form:"sort"`
	// CreatedAfter through ExpiresBefore bound the instants they name; zero
	// bounds nothing.
	CreatedAfter   time.Time `form:"created_after"`
	CreatedBefore  time.Time `form:"created_before"`
	CanceledAfter  time.Time `form:"canceled_after"`
	CanceledBefore time.Time `form:"canceled_before"`
	ExpiresBefore  time.Time `form:"expires_before"`
	// ScheduledChangeBefore keeps the subscriptions whose MRR is scheduled to
	// change by then: a cancellation at period end, or a scheduled price or
	// seat change, effective at or before it.
	ScheduledChangeBefore time.Time `form:"scheduled_change_before"`
	// AtRisk keeps the subscriptions whose MRR is at risk; RiskReason keeps
	// those at risk for that reason.
	AtRisk     bool          `form:"at_risk"`
	RiskReason MRRRiskReason `form:"risk_reason"`
}

// Subscription is one subscription's lifecycle and recovery state. The
// merchant routes and the customer's /v1/me/subscriptions routes serve this
// one shape; the self routes also fill ScheduledChange's Price and Product,
// CancelPortalURL and Access.
type Subscription struct {
	// CollectionPolicy is read-only scheduling/recovery ownership.
	CollectionPolicy string           `json:"collection_policy"`
	Recovery         *PaymentRecovery `json:"recovery,omitempty"`
	// Dunning is the declined renewal being collected; null unless the
	// subscription is past_due or awaiting_method.
	Dunning             *SubscriptionDunning `json:"dunning"`
	DeletionScheduledAt *time.Time           `json:"deletion_scheduled_at,omitempty"`
	ID                  SubscriptionID       `json:"id"`
	CustomerID          CustomerID           `json:"customer_id"`
	// CustomerContact is the customer's directory contact on a staff list;
	// absent when the directory holds none or could not answer.
	CustomerContact *CustomerContact `json:"customer_contact,omitempty"`
	ProductID       ProductID        `json:"product_id"`
	PriceID         PriceID          `json:"price_id"`
	// Quantity is the seats of a per-seat price: each period bills the unit
	// amount times Quantity. Null unless the price is sold per seat.
	Quantity *int `json:"quantity"`
	// MRR is what the subscription adds to monthly recurring revenue, with
	// its scheduled change; the merchant routes fill it, the customer's own
	// routes leave it null.
	MRR *SubscriptionMRR `json:"mrr"`
	// MRRRisk is why the subscription's MRR is at risk, or null; the
	// merchant routes fill it.
	MRRRisk            *MRRRisk           `json:"mrr_risk"`
	PSPID              PSPID              `json:"psp_id"`
	Rail               Rail               `json:"rail"`
	RailSubscriptionID *string            `json:"rail_subscription_id"`
	Status             SubscriptionStatus `json:"status"`
	// UnverifiedReason is why an unverified subscription is; null otherwise.
	UnverifiedReason *UnverifiedReason `json:"unverified_reason"`
	// ScheduledChange is the change waiting for the next renewal, or null.
	ScheduledChange *ScheduledChange `json:"scheduled_change"`
	PaymentMethodID *PaymentMethodID `json:"payment_method_id"`
	// MandateID is the agreement the subscription renews under; null when it
	// has none (a provider-owned subscription, or one waiting for a card).
	MandateID             *MandateID      `json:"mandate_id"`
	StartedAt             time.Time       `json:"started_at"`
	EndedAt               *time.Time      `json:"ended_at"`
	CurrentPeriodStartsAt *time.Time      `json:"current_period_starts_at"`
	CurrentPeriodEndsAt   *time.Time      `json:"current_period_ends_at"`
	CanceledAt            *time.Time      `json:"canceled_at"`
	CancelType            *string         `json:"cancel_type"`
	CancelFeedback        *string         `json:"cancel_feedback"`
	Resumable             bool            `json:"resumable"`
	CancelScheduled       bool            `json:"cancel_scheduled"`
	CancelMode            string          `json:"cancel_mode"`
	Price                 *Price          `json:"price,omitempty"`
	Product               *ProductSummary `json:"product,omitempty"`
	// Card is display data for the card the subscription charges: its own
	// (PaymentMethodID), or, when PaymentMethodID is null on a card
	// subscription, the customer's default card for its currency.
	Card *CardDetails `json:"card,omitempty"`
	// CancelPortalURL is where the customer cancels when CancelMode is
	// external_portal (rails that keep cancellation on their own site).
	CancelPortalURL *string `json:"cancel_portal_url,omitempty"`
	// Access summarizes the premium access this subscription grants; the self
	// routes fill it.
	Access *SubscriptionAccess `json:"access,omitempty"`
	// NextAction is set only on the answer to an action that asks for the
	// customer's own step (a Solana cancel's optional on-chain cancel, signed
	// by the customer's wallet). Complete the step and repeat the request with
	// its result.
	NextAction *NextAction `json:"next_action"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

// SubscriptionMRR is a subscription's monthly recurring revenue: its price's
// list amount normalised to a month, times its seats, in the price's
// currency. A pending subscription, one in a trial and one that has ended add
// zero; one canceled at period end adds its MRR until the end.
type SubscriptionMRR struct {
	Amount   int64            `json:"amount,string"`
	Currency catalog.Currency `json:"currency"`
	// Scheduled is the change waiting for a date, or null.
	Scheduled *ScheduledMRR `json:"scheduled"`
}

// ScheduledMRRKind is what a scheduled change does to MRR.
type ScheduledMRRKind string

const (
	// ScheduledMRRChurn is a cancellation at period end: the subscription
	// leaves MRR at its end.
	ScheduledMRRChurn ScheduledMRRKind = "churn"
	// ScheduledMRRContraction is a scheduled change to a lower price or fewer
	// seats, from the renewal it applies at.
	ScheduledMRRContraction ScheduledMRRKind = "contraction"
	// ScheduledMRRExpansion is a scheduled change to a higher price or more
	// seats.
	ScheduledMRRExpansion ScheduledMRRKind = "expansion"
)

// ScheduledMRR is a change of a subscription's MRR waiting for EffectiveAt.
// Amount is the change: negative for churn and contraction.
type ScheduledMRR struct {
	Kind        ScheduledMRRKind `json:"kind"`
	Amount      int64            `json:"amount,string"`
	EffectiveAt time.Time        `json:"effective_at"`
}

// MRRRiskReason is why a subscription's MRR is at risk.
type MRRRiskReason string

const (
	// MRRRiskPastDue is a declined renewal being retried.
	MRRRiskPastDue MRRRiskReason = "past_due"
	// MRRRiskNeedsPaymentMethod is a declined renewal that waits for a new
	// card: awaiting_method, or a provider's past due whose latest decline
	// asks for one.
	MRRRiskNeedsPaymentMethod MRRRiskReason = "needs_payment_method"
	// MRRRiskUnverified is a subscription whose provider state is unknown.
	MRRRiskUnverified MRRRiskReason = "unverified"
	// MRRRiskCardExpiring is a card OpenRails charges that expires before the
	// renewal it pays for.
	MRRRiskCardExpiring MRRRiskReason = "card_expiring"
	// MRRRiskDelinquent is a customer delinquent in the subscription's
	// currency: overdue usage past its grace.
	MRRRiskDelinquent MRRRiskReason = "delinquent"
)

// MRRRiskReasons is every MRRRiskReason, in the order a subscription lists
// them.
func MRRRiskReasons() []MRRRiskReason {
	return []MRRRiskReason{MRRRiskPastDue, MRRRiskNeedsPaymentMethod, MRRRiskUnverified, MRRRiskCardExpiring, MRRRiskDelinquent}
}

// MRRRisk is a subscription's MRR at risk: its reasons now, and the case
// they belong to, which began at Since for EntryReason. The at-risk amount is
// the subscription's MRR.
type MRRRisk struct {
	Reasons     []MRRRiskReason `json:"reasons"`
	EntryReason MRRRiskReason   `json:"entry_reason"`
	Since       time.Time       `json:"since"`
}

// SubscriptionDunning is a declined renewal's collection. Attempts counts
// the renewal's declined charges so far. RetriesLeft and FinalRetryAt follow the
// dunning policy the case opened under: the charges still to come if each
// declines, and when the last one runs; both are null when the provider runs
// the retries, and NextRetryAt is then the provider's next attempt when
// OpenRails knows it. NextRetryAt is null while nothing is scheduled. A subscription
// WaitingForNewCard is charged again only once the customer replaces the
// card, and is canceled at FinalRetryAt if none arrives. LastFailureReason is
// the latest decline's reason, null when none was recorded.
type SubscriptionDunning struct {
	// Paused means the merchant's write posture holds OpenRails' automatic
	// retries. The dates below remain the planned schedule, not a promise to
	// charge. False does not bypass other payment or provider checks.
	Paused bool `json:"paused"`
	// PauseReason is automatic_collection_paused (a limited write posture) or
	// provider_writes_disabled (readonly). A full posture clears this hold; a
	// provider-owned retry schedule is not paused by OpenRails' posture.
	PauseReason       string         `json:"pause_reason,omitempty"`
	Attempts          int            `json:"attempts"`
	RetriesLeft       *int           `json:"retries_left"`
	NextRetryAt       *time.Time     `json:"next_retry_at"`
	FinalRetryAt      *time.Time     `json:"final_retry_at"`
	WaitingForNewCard bool           `json:"waiting_for_new_card"`
	LastFailureReason *DeclineReason `json:"last_failure_reason"`
}

// SubscriptionAccess is how a customer currently holds premium access: Kind
// is subscription (the subscription itself) or entitlement (a standing
// entitlement window, e.g. a one-off purchase or an admin grant). SourceID is
// the source's own wire id: a SubscriptionID for a subscription or grace
// source, a PaymentID for a purchase, the host's declared id otherwise.
type SubscriptionAccess struct {
	Kind           string         `json:"kind"`
	ProductID      ProductID      `json:"product_id"`
	SourceType     string         `json:"source_type,omitempty"`
	SourceID       string         `json:"source_id,omitempty"`
	SubscriptionID SubscriptionID `json:"subscription_id,omitzero"`
	Rail           Rail           `json:"rail,omitempty"`
	StartsAt       time.Time      `json:"starts_at"`
	EndsAt         *time.Time     `json:"ends_at,omitempty"`
}

// ProductSummary names the product a subscription or payment is for. It is
// not the catalog Product: that one carries the product's current prices and
// entitlement spec, which are not about this subscription or payment, whose
// own price is beside it.
type ProductSummary struct {
	ID          ProductID `json:"id"`
	Key         string    `json:"key"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`
	TierGroup   *string   `json:"tier_group"`
	TierRank    int       `json:"tier_rank"`
	Archived    bool      `json:"archived"`
}

// CancelSubscriptionParams is the merchant's cancel: at period end (access
// kept for its purchased duration) or, with RevokeAccess, immediately. Both stop
// provider billing.
type CancelSubscriptionParams struct {
	Reason       string `json:"reason"`
	RevokeAccess bool   `json:"revoke_access,omitempty"`
	// AccountDeletion marks the host's irrevocable account-deletion cancel.
	// While destructive provider actions are disarmed an ordinary cancel of a
	// provider-billed subscription is refused (CodeProviderCancelHeld); an
	// account deletion is canceled locally and its provider schedule delete
	// waits for the operator's arming.
	AccountDeletion bool `json:"account_deletion,omitempty"`
}

// SetSubscriptionPaymentMethodParams gives a subscription its own card, another
// of the customer's saved cards; null clears it so the subscription follows
// the customer's default card for its currency.
type SetSubscriptionPaymentMethodParams struct {
	PaymentMethodID *PaymentMethodID `json:"payment_method_id"`
}

// CodeCustomerActionRequired: only the customer can take this action, through
// their own step (a wallet signature, the provider's hosted page, an
// authenticated payment). Nothing changed.
const CodeCustomerActionRequired ErrorCode = "customer_action_required"

// CodePaymentMethodPSPMismatch: the saved method the request named was vaulted
// by a different provider account than the one that owns the subscription
// (type invalid_request_error, 409). Provider vault references are
// account-scoped, so nothing was sent to the provider; collect the card again
// on the subscription's active provider account.
const CodePaymentMethodPSPMismatch ErrorCode = "payment_method_psp_mismatch"

var ErrPaymentMethodPSPMismatch error = newCodedError(CodePaymentMethodPSPMismatch, ErrConflict)
