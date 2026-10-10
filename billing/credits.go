package billing

import (
	"time"

	"github.com/google/uuid"
)

// CreditGrantID names one credit grant: a lot of prepaid credit.
type CreditGrantID uuid.UUID

// BalanceTransactionID names one movement on a customer's balance.
type BalanceTransactionID uuid.UUID

const (
	CreditGrantIDPrefix        = "cgr_"
	BalanceTransactionIDPrefix = "txn_"
)

func ParseCreditGrantID(s string) (CreditGrantID, error) {
	u, err := parsePrefixedID("credit grant", CreditGrantIDPrefix, s)
	return CreditGrantID(u), err
}
func (id CreditGrantID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id CreditGrantID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id CreditGrantID) String() string  { return formatPrefixedID(CreditGrantIDPrefix, uuid.UUID(id)) }
func (id CreditGrantID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}
func (id *CreditGrantID) UnmarshalText(text []byte) error {
	parsed, err := ParseCreditGrantID(string(text))
	*id = parsed
	return err
}

func ParseBalanceTransactionID(s string) (BalanceTransactionID, error) {
	u, err := parsePrefixedID("balance transaction", BalanceTransactionIDPrefix, s)
	return BalanceTransactionID(u), err
}
func (id BalanceTransactionID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id BalanceTransactionID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id BalanceTransactionID) String() string {
	return formatPrefixedID(BalanceTransactionIDPrefix, uuid.UUID(id))
}
func (id BalanceTransactionID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}
func (id *BalanceTransactionID) UnmarshalText(text []byte) error {
	parsed, err := ParseBalanceTransactionID(string(text))
	*id = parsed
	return err
}

// CreateCreditGrantParams grants a customer prepaid credit. SourceID identifies the
// grant per customer and must be reproducible across retries: an identical
// retry returns the existing grant (Replayed), one whose amount, currency or
// expiry differs is ErrIdempotencyKeyReused. Source labels where the money
// came from; Invoker records who granted it (default: the customer).
type CreateCreditGrantParams struct {
	CustomerID  CustomerID `json:"customer_id"`
	Currency    string     `json:"currency"`
	Amount      int64      `json:"amount,string"`
	SourceID    string     `json:"source_id"`
	Source      string     `json:"source"`
	Invoker     string     `json:"invoker,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Description *string    `json:"description,omitempty"`
}

// CreditGrantState is where a credit grant stands.
type CreditGrantState string

const (
	CreditGrantActive    CreditGrantState = "active"
	CreditGrantScheduled CreditGrantState = "scheduled"
	CreditGrantSpent     CreditGrantState = "spent"
	CreditGrantExpired   CreditGrantState = "expired"
	CreditGrantRevoked   CreditGrantState = "revoked"
	// CreditGrantTerminated was superseded by a later grant.
	CreditGrantTerminated CreditGrantState = "terminated"
)

// CreditGrant is one lot of prepaid credit on a customer's balance, spent
// first-in first-out. SourceType says what created it (admin for a merchant
// grant, purchase, subscription); SourceID is that source's id. Replayed: a
// create that found the grant already made.
type CreditGrant struct {
	ID                CreditGrantID    `json:"id"`
	CustomerID        CustomerID       `json:"customer_id"`
	Currency          string           `json:"currency"`
	Amount            int64            `json:"amount,string"`
	SpentAmount       int64            `json:"spent_amount,string"`
	RemainingAmount   int64            `json:"remaining_amount,string"`
	RevokedAmount     int64            `json:"revoked_amount,string"`
	ExpiredAmount     int64            `json:"expired_amount,string"`
	State             CreditGrantState `json:"state"`
	SourceType        string           `json:"source_type"`
	SourceID          *string          `json:"source_id"`
	Description       *string          `json:"description"`
	StartsAt          time.Time        `json:"starts_at"`
	ExpiresAt         *time.Time       `json:"expires_at"`
	CreatedAt         time.Time        `json:"created_at"`
	TerminatedAt      *time.Time       `json:"terminated_at"`
	TerminationReason *string          `json:"termination_reason"`
	Replayed          bool             `json:"replayed"`
}

// CreditGrantListParams filters a customer's credit grants, newest first.
type CreditGrantListParams struct {
	// CustomerID is whose grants to list; required unless IDs is set.
	CustomerID CustomerID `form:"-"`
	Currency   string     `form:"currency"`
	SourceID   string     `form:"source_id"`
	// IDs instead reads 1 to MaxBatchItems named grants in one page, whatever
	// their state; unknown ones are absent.
	IDs []CreditGrantID `form:"-"`
	PageRequest
}

// CreateCreditGrantBatchParams grants 1 to MaxBatchItems credits, across any
// customers, all or none.
type CreateCreditGrantBatchParams struct {
	Items []CreateCreditGrantParams `json:"items"`
}

// CreateCreditGrantBatchResult is every grant, in request order.
type CreateCreditGrantBatchResult struct {
	Items []CreditGrant `json:"items"`
}

// RevokeCreditGrantParams revokes a grant's unspent remainder. Revoking a
// revoked grant returns it (Replayed).
type RevokeCreditGrantParams struct {
	Reason string `json:"reason"`
}

// BalanceTransactionType is what a balance movement did.
type BalanceTransactionType string

const (
	BalanceTransactionDeposit   BalanceTransactionType = "deposit"
	BalanceTransactionSpend     BalanceTransactionType = "spend"
	BalanceTransactionExpire    BalanceTransactionType = "expire"
	BalanceTransactionRevoke    BalanceTransactionType = "revoke"
	BalanceTransactionReinstate BalanceTransactionType = "reinstate"
	// The owed_ types move what the customer owes in arrears.
	BalanceTransactionOwedAccrual  BalanceTransactionType = "owed_accrual"
	BalanceTransactionOwedPayment  BalanceTransactionType = "owed_payment"
	BalanceTransactionOwedWriteoff BalanceTransactionType = "owed_writeoff"
	// BalanceTransactionOwedRepayment pays debt from newly funded credit; Amount is the
	// (negative) change to both the balance and what is owed.
	BalanceTransactionOwedRepayment BalanceTransactionType = "owed_repayment"
)

// BalanceTransaction is one movement on a customer's balance. Amount is
// signed: the change to the balance, or for owed_ types to what is owed.
type BalanceTransaction struct {
	ID            BalanceTransactionID   `json:"id"`
	CustomerID    CustomerID             `json:"customer_id"`
	Currency      string                 `json:"currency"`
	Type          BalanceTransactionType `json:"type"`
	Amount        int64                  `json:"amount,string"`
	CreditGrantID *CreditGrantID         `json:"credit_grant_id"`
	Invoker       *string                `json:"invoker"`
	Resource      *string                `json:"resource"`
	Source        string                 `json:"source"`
	SourceID      string                 `json:"source_id"`
	CreatedAt     time.Time              `json:"created_at"`
}

// BalanceTransactionListParams selects a customer's ledger in one currency,
// newest first. IDs instead reads 1 to MaxBatchItems of the customer's named
// transactions in one page, in any currency; unknown ones are absent.
type BalanceTransactionListParams struct {
	Currency string                 `form:"currency"`
	IDs      []BalanceTransactionID `form:"-"`
	PageRequest
}

// BillingMode is how a customer pays for spend: from prepaid balance, or
// accrued as owed and invoiced.
type BillingMode string

const (
	BillingModePrepaid BillingMode = "prepaid"
	BillingModeArrears BillingMode = "arrears"
)

// Balance is a customer's money in one currency. AvailableAmount is
// BalanceAmount less HeldAmount; OwedAmount is unpaid arrears.
type Balance struct {
	CustomerID      CustomerID  `json:"customer_id"`
	Currency        string      `json:"currency"`
	BillingMode     BillingMode `json:"billing_mode"`
	BalanceAmount   int64       `json:"balance_amount,string"`
	HeldAmount      int64       `json:"held_amount,string"`
	AvailableAmount int64       `json:"available_amount,string"`
	OwedAmount      int64       `json:"owed_amount,string"`
}

// BudgetWindow caps spend at Limit per WindowSeconds, in Currency (the
// request's currency when empty). A spend window is at most 31 days.
type BudgetWindow struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit,string"`
	Currency      string `json:"currency,omitempty"`
}

// MerchantProfile is the merchant's public and communication metadata; its
// name is the merchant's display_name.
type MerchantProfile struct {
	LogoURL    string `json:"logo_url,omitempty"`
	FromEmail  string `json:"from_email,omitempty"`
	SupportURL string `json:"support_url,omitempty"`
	SignupURL  string `json:"signup_url,omitempty"`
}

// MerchantSettings is the merchant-owned admission/policy document installed by
// standalone policy sync jobs.
type MerchantSettings struct {
	Profile                    *MerchantProfile `json:"profile,omitempty"`
	InvoiceCollectionThreshold *int64           `json:"collection_threshold,omitempty,string"`
	InvoiceMonthlyFloor        *int64           `json:"monthly_floor,omitempty,string"`
	InvoiceBillingBoundary     string           `json:"billing_period_boundary,omitempty"`
	AlertEmail                 *string          `json:"alert_email,omitempty"`
	RepriceNoticeWindowDays    *int             `json:"reprice_notice_window_days,omitempty"`
	// RenewalReceiptMinIntervalHours spaces renewal receipts per subscription:
	// a renewal starting sooner than this after the membership start or the
	// last receipted renewal sends none. Nil uses 24; 0 receipts every renewal.
	RenewalReceiptMinIntervalHours *int `json:"renewal_receipt_min_interval_hours,omitempty"`
	// ProviderRefundAccess decides what a refund made in the provider's own
	// dashboard does to access, on every rail: ProviderRefundRevokeOnFull
	// (default), ProviderRefundRevokeOnAny or ProviderRefundKeep. Refunds made
	// through OpenRails follow their own revoke_access choice.
	ProviderRefundAccess    *string                `json:"provider_refund_access,omitempty"`
	ArrearsGraceDays        *int                   `json:"arrears_grace_days,omitempty"`
	ArrearsDelinquencyFloor *int64                 `json:"arrears_delinquency_floor,omitempty,string"`
	CheckoutRouting         *[]CheckoutRoutingRule `json:"checkout_routing,omitempty"`
	// DunningPolicy replaces the built-in dunning schedule. Nil keeps the
	// stored policy; the built-in one applies until a merchant declares one.
	DunningPolicy *DunningPolicy `json:"dunning_policy,omitempty"`
	// BillingPolicies / BillingPolicyBindings are the or#897 registry: named
	// policies and the rungs that decide who gets which. They REPLACE the retired
	// trust_level_spend_limits field, which could only ever mean "window cap".
	BillingPolicies                   []BillingPolicy        `json:"billing_policies,omitempty"`
	BillingPolicyBindings             []BillingPolicyBinding `json:"billing_policy_bindings,omitempty"`
	DelegatedInvokerWastedSpendLimits []BudgetWindow         `json:"delegated_invoker_wasted_spend_limits,omitempty"`
}

// BillingPolicy declares one named billing policy (or#897). The policy says
// WHICH quantity is capped; the binding says who it applies to.
type BillingPolicy struct {
	Name string `json:"name"`
	// Kind is "outstanding_cap" (cap LEDGER-measured unpaid arrears — a credit
	// line on debt, refused with outstanding_cap_reached), "window_spend_cap"
	// (cap NEW spend per rolling window; prior debt drives delinquency, not
	// admission), or "accrual_rate_cap" (cap the measured accrual RATE in
	// micros/hour — the cloud quota, refused with accrual_rate_cap_reached).
	Kind string `json:"kind"`
	// OutstandingCapAmount (micros) is the credit line for kind=outstanding_cap.
	// Zero defers to the payer's own arrears credit limit.
	OutstandingCapAmount int64 `json:"outstanding_cap_amount,omitempty,string"`
	// SpendWindows are the rolling NEW-spend ceilings for kind=window_spend_cap.
	SpendWindows []BudgetWindow `json:"spend_windows,omitempty"`
	// AccrualRateCapPerHour (kind=accrual_rate_cap) caps the measured accrual
	// rate in micros PER HOUR — the cloud quota. AccrualRateWindowSeconds is the
	// measurement lookback (default 3600).
	AccrualRateCapPerHour    int64 `json:"accrual_rate_cap_per_hour,omitempty,string"`
	AccrualRateWindowSeconds int64 `json:"accrual_rate_window_seconds,omitempty"`
	// CollectionThresholdAmount / DelinquencyGraceDays / DelinquencyAmountFloor
	// override the merchant-wide invoice policy for payers bound here; nil defers
	// to it. All three ride on any kind.
	CollectionThresholdAmount *int64 `json:"collection_threshold_amount,omitempty,string"`
	DelinquencyGraceDays      *int   `json:"delinquency_grace_days,omitempty"`
	DelinquencyAmountFloor    *int64 `json:"delinquency_amount_floor,omitempty,string"`
	// CollectionCycleBoundary is declarable and REFUSED: statement periods must
	// tile a payer's lifetime, and rebinding is a live lever, so the boundary
	// stays merchant-wide. Declaring it here fails with that reason.
	CollectionCycleBoundary string `json:"collection_cycle_boundary,omitempty"`
	// BadSpendWindows are the #497 per-PAYER direct-credential wasted-spend grace
	// windows: at most Limit of host-reported wasted spend is forgiven per window;
	// direct-payer overage is charged. Allowed on either kind.
	BadSpendWindows []BudgetWindow `json:"bad_spend_windows,omitempty"`
	PolicyCurrency  string         `json:"policy_currency,omitempty"`
}

// BillingPolicyBinding declares a merchant-default or per-tier policy.
// Customer assignments are runtime state managed by SetCustomerBillingPolicy.
type BillingPolicyBinding struct {
	PolicyName string `json:"policy"`
	Tier       string `json:"tier,omitempty"`
}

// DunningPolicy is a merchant's retry schedule for declined renewals. A
// dunning case runs under the policy in force at its first decline; an edit
// applies to the next case. Tiers (default: the built-in schedule) are
// ordered by billing cycle: the first tier whose MaxCycleHours exceeds a
// subscription's cycle applies, and the last tier (MaxCycleHours 0) takes
// every longer cycle. RetryAfterHours are measured from the first decline.
// TransientRetryMinutes is the quick ladder for processor try-again answers,
// which does not count as dunning failures.
type DunningPolicy struct {
	Tiers                 []DunningTier `json:"tiers"`
	TransientRetryMinutes []int         `json:"transient_retry_minutes,omitempty"`
	// AccessDuringDunning is DunningAccessKeep (default: members keep access
	// until a confirmed outcome ends it) or DunningAccessSuspend (access ends
	// with the paid period while a declined renewal is retried).
	AccessDuringDunning string `json:"access_during_dunning,omitempty"`
	// AccessWhileRenewalHeld is DunningAccessKeep (default: a renewal with no
	// outcome because collection is stopped keeps access until it is
	// attempted) or DunningAccessSuspend (access ends at the renewal
	// allowance, min(24h, max(5m, period/10)) past the paid period).
	AccessWhileRenewalHeld string `json:"access_while_renewal_held,omitempty"`
}

// Dunning access policies.
const (
	DunningAccessKeep    = "keep"
	DunningAccessSuspend = "suspend"
)

// DunningTier is the retry schedule for cycles shorter than MaxCycleHours.
type DunningTier struct {
	MaxCycleHours   int   `json:"max_cycle_hours,omitempty"`
	RetryAfterHours []int `json:"retry_after_hours,omitempty"`
}
