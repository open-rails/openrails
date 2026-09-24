// Package openrails is the canonical OpenRails SDK surface (#338): ONE Go
// client implementation with two constructors —
//
//   - NewRemote(baseURL, opts...) talks to a standalone OpenRails over its
//     service-credential-authenticated /v1/merchant/* routes (this file + remote.go,
//     ported from the go-client module, which this package supersedes);
//   - openrails/embed.New(...).Client() runs the engine in-process and returns
//     the SAME client implementation wired to an in-process transport (#685): a
//     custom http.RoundTripper dispatching into the neutral /v1/merchant
//     handler, no socket.
//
// PARITY IS STRUCTURAL: one client implementation, one handler surface — the
// transports cannot drift because there is nothing to drift between. The
// dual-mode conformance test in openrails/embed enforces this end to end.
//
// This root package stays dependency-light: it must not link the engine or
// pkg/embedded. The shared, standard-library-only archive format verifier is
// the sole internal-package exception (enforced by deps_test.go).
package openrails

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	// InvokerTypeDelegated marks an invoker as a third-party/member/federated
	// principal using the payer's billing authority. Flat invoker waste cutoffs
	// apply.
	InvokerTypeDelegated = "delegated"
	// InvokerTypePayer marks an invoker as a direct payer-controlled credential.
	// Wasted-spend reports use payer grace, then charge overage.
	InvokerTypePayer = "payer"
)

// MerchantID is an OpenRails merchant identifier.
type MerchantID = merchant.ID

// SelfIssuer is the issuer keying customers rows for self-service
// identities whose subject is the user's own UUID — what an embedded host
// passes to ListActiveEntitlements for its own users (internal/db
// EnsureCustomerID materializes rows under it).
const SelfIssuer = "openrails:self"

// DepositCreditsRequest mints a credit block for a payer (admin funding,
// promotions, money-in settlement). Amount is in the currency's native integer unit.
type DepositCreditsRequest struct {
	CustomerID *string `json:"customer_id"`
	Invoker    string  `json:"invoker"`
	Currency   string  `json:"currency"`
	// Amount is the deposit size in the currency's internal precision (micros for USD).
	Amount int64 `json:"amount,string"`
	// Source identifies the system of record for this deposit (e.g. "stripe", "manual").
	Source string `json:"source"`
	// SourceID is the idempotency key for the deposit. REQUIRED, and it must be
	// REPRODUCIBLE by the caller across retries of the same logical deposit —
	// deriving it from the operation's own identity is the only way it survives
	// this process. A value minted per request (uuid.New() in a handler) passes
	// validation and guarantees nothing: it is a new deposit every time, which
	// is exactly how a retried admin deposit double-credited an org. Any
	// non-empty string (or#906; no longer restricted to a UUID).
	//
	// The deposit key is (merchant, payer, SourceID), UNIQUE in the database
	// (or#906). Source is NOT part of it — doctrine, restated deliberately: the
	// same SourceID under a different Source is still the same deposit, so a
	// retry that relabels its source cannot double-credit. An IDENTICAL replay
	// is answered with the EXISTING grant (Replayed=true); a replay whose
	// amount, unit or expiry differs is refused with ErrIdempotencyKeyReused (HTTP 409).
	SourceID    string     `json:"source_id"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Description string     `json:"description"`
}

// CreditTransaction is the canonical ledger receipt shared by both transports.
// Field names are snake_case, amounts are decimal strings in the currency's
// native integer precision, and timestamps are RFC3339 instants.
type CreditTransaction struct {
	ID              uuid.UUID  `json:"id"`
	CustomerID      string     `json:"customer_id"`
	Invoker         string     `json:"invoker"`
	Currency        string     `json:"currency"`
	Amount          int64      `json:"amount,string"`
	BalanceAfter    *int64     `json:"balance_after,string"`
	TransactionType string     `json:"transaction_type"`
	Status          string     `json:"status"`
	Authorized      *int64     `json:"authorized,string"`
	Captured        *int64     `json:"captured,string"`
	Source          string     `json:"source"`
	SourceID        *string    `json:"source_id"`
	ExpiresAt       *time.Time `json:"expires_at"`
	Description     *string    `json:"description"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	// Replayed reports that this write's idempotency key had ALREADY committed,
	// so nothing moved in THIS call — the row described here is the movement
	// that landed earlier (or#892). Serialized by the engine on both transports;
	// a consumer that needs applied-vs-replayed reads it here instead of keeping
	// its own claim table.
	Replayed bool `json:"replayed"`
}

// AdmitRequest is one item in POST /v1/merchant/admissions. It checks payer money
// capacity, delegated spend policy, delegated wasted-spend cutoff, and places the
// request hold when allowed.
//
// TrustLevel selects money policy. Resource is host-side attribution only;
// endpoint authorization stays with the host.
// EstimatedAmount is the upper-bound charge to hold. A zero EstimatedAmount runs
// the limit checks without placing a money hold.
type AdmitRequest struct {
	CustomerID      string `json:"customer_id"`
	Invoker         string `json:"invoker"`
	InvokerType     string `json:"invoker_type,omitempty"`
	TrustLevel      string `json:"trust_level,omitempty"`
	Resource        string `json:"resource,omitempty"`
	Currency        string `json:"currency,omitempty"`
	EstimatedAmount int64  `json:"estimated_amount,string"`
	// AccrualRateDeltaPerHour is the or#897 PROSPECTIVE rate this request would
	// add, in micros per hour — "the VM I am about to start burns $2/hour". Only
	// the host knows it. Zero means the request adds no ongoing rate, which
	// leaves an accrual_rate_cap payer gated on what is already running.
	AccrualRateDeltaPerHour int64  `json:"accrual_rate_delta_per_hour,omitempty,string"`
	RequestID               string `json:"request_id"`
	Source                  string `json:"source,omitempty"`
	// ExpiresAt is the deadline of the job this admit covers. REQUIRED when
	// EstimatedAmount places a hold: the hold lives that long unless captured,
	// released or extended (ExtendHold). Refused otherwise.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Roles are the immutable role UUIDs the invoker holds (#473). Each role with a
	// matching (subject, role) budget-scope policy gates this request's spend in
	// the same admit verdict. The host reads them from the delegated
	// JWT/permission set. Empty = no role-scoped budget applies.
	Roles []uuid.UUID `json:"roles,omitempty"`
}

// AdmitResponse is the admission verdict (internal/service.AdmitResult on the wire).
// Allowed=false carries a BlockedBy axis ("budget" | "abuse" | "money") and a
// DenyCode when available. A successful money-bearing admit creates a request_id
// keyed SQL operation. A deny is returned as (Allowed=false, nil error) on both
// transports even though HTTP maps it to 402/403/429.
type AdmitResponse struct {
	// Allowed preserves the original decision on replay. Use Active to decide
	// whether the operation still has a live reservation; terminal receipts are not new authority.
	Allowed             bool       `json:"allowed"`
	BlockedBy           string     `json:"blocked_by,omitempty"`
	DenyCode            string     `json:"deny_code,omitempty"`
	Currency            string     `json:"currency,omitempty"`
	EstimatedAmount     int64      `json:"estimated_amount,omitempty,string"`
	StartCapacityAmount int64      `json:"start_capacity_amount,omitempty,string"`
	RetryAfterSeconds   int64      `json:"retry_after_seconds,omitempty"`
	HoldExpiresAt       *time.Time `json:"hold_expires_at,omitempty"`
	Replayed            bool       `json:"replayed"`
	State               string     `json:"state,omitempty"`
}

// Active reports a currently open, originally allowed admission. A denied
// result has no operation state; an expired or terminal replay is never active.
func (r *AdmitResponse) Active() bool { return r != nil && r.Allowed && r.State == "open" }

// CaptureUsage carries the analytics dimensions recorded alongside a capture so
// OpenRails can serve per-resource/function/tier/invoker spend (#410). Nil = no
// usage event (a plain capture). Usage and the financial capture commit together.
// The first capture fixes these terms; a changed retry returns ErrIdempotencyKeyReused.
type CaptureUsage struct {
	// EventType classifies the usage event (e.g. "inference", "storage"). Required
	// for the event to be recorded; a blank EventType suppresses the usage event.
	EventType string `json:"event_type,omitempty"`
	// Resource is the host-defined resource attribution key (e.g. model name, endpoint).
	Resource string `json:"resource,omitempty"`
	// Metadata holds arbitrary key/value dimensions for analytics rollups.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Source identifies the system that generated this usage event.
	Source string `json:"source,omitempty"`
	// SourceID is the idempotency key for this usage event within the Source namespace.
	SourceID   string           `json:"source_id,omitempty"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}

// BalanceResponse is the GET /v1/merchant/credits/balance snapshot (handler
// serviceBalanceResponse). NOTE: the wire field for the owed amount is
// outstanding_owed_amount.
type BalanceResponse = CreditAccount

// CreditAccount is the OpenRails service balance/policy snapshot for one
// customer + currency pair. All amounts are in the currency's internal
// precision (micros for USD).
type CreditAccount struct {
	CustomerID  string `json:"customer_id"`
	Currency    string `json:"currency"`
	BillingMode string `json:"billing_mode"`
	// BalanceAmount is the total prepaid credit balance (excluding holds).
	BalanceAmount int64 `json:"balance_amount,string"`
	// HeldAmount is the sum of outstanding authorization holds not yet captured or released.
	HeldAmount int64 `json:"held_amount,string"`
	// AvailableAmount is BalanceAmount minus HeldAmount — the credit available for new admits.
	AvailableAmount int64 `json:"available_amount,string"`
	// OutstandingOwedAmount is the unpaid postpaid balance (postpaid billing mode only).
	OutstandingOwedAmount int64 `json:"outstanding_owed_amount,string"`
}

// UsageRollupRow is one grouped spend bucket from OpenRails.
type UsageRollupRow struct {
	Key         string `json:"key"`
	Currency    string `json:"currency"`
	EventCount  int64  `json:"event_count"`
	TotalAmount int64  `json:"total_amount,string"`
}

// BudgetWindowInput is a caller-supplied fixed budget window sent to
// OpenRails. The host owns the policy; OpenRails owns the spend actuals.
type BudgetWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit,string"`
	Currency      string `json:"currency,omitempty"`
}

// MerchantProfileInput is public/communication metadata stored per merchant.
type MerchantProfileInput struct {
	DisplayName string `json:"display_name,omitempty"`
	LogoURL     string `json:"logo_url,omitempty"`
	FromEmail   string `json:"from_email,omitempty"`
	SupportURL  string `json:"support_url,omitempty"`
	SignupURL   string `json:"signup_url,omitempty"`
}

// MerchantSettings is the merchant-owned admission/policy document installed by
// standalone policy sync jobs.
type MerchantSettings struct {
	Profile                    *MerchantProfileInput `json:"profile,omitempty"`
	InvoiceCollectionThreshold *int64                `json:"collection_threshold,omitempty,string"`
	InvoiceMonthlyFloor        *int64                `json:"monthly_floor,omitempty,string"`
	InvoiceBillingBoundary     string                `json:"billing_period_boundary,omitempty"`
	AlertEmail                 *string               `json:"alert_email,omitempty"`
	RepriceNoticeWindowDays    *int                  `json:"reprice_notice_window_days,omitempty"`
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
	// BillingPolicies / BillingPolicyBindings are the or#897 registry: named
	// policies and the rungs that decide who gets which. They REPLACE the retired
	// trust_level_spend_limits field, which could only ever mean "window cap".
	BillingPolicies                   []BillingPolicyInput        `json:"billing_policies,omitempty"`
	BillingPolicyBindings             []BillingPolicyBindingInput `json:"billing_policy_bindings,omitempty"`
	DelegatedInvokerWastedSpendLimits []BudgetWindowInput         `json:"delegated_invoker_wasted_spend_limits,omitempty"`
}

// BillingPolicyInput declares one named billing policy (or#897). The policy says
// WHICH quantity is capped; the binding says who it applies to.
type BillingPolicyInput struct {
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
	SpendWindows []BudgetWindowInput `json:"spend_windows,omitempty"`
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
	BadSpendWindows []BudgetWindowInput `json:"bad_spend_windows,omitempty"`
	PolicyCurrency  string              `json:"policy_currency,omitempty"`
}

// BillingPolicyBindingInput declares a merchant-default or per-tier policy.
// Customer assignments are runtime state managed by SetCustomerBillingPolicy.
type BillingPolicyBindingInput struct {
	PolicyName string `json:"policy"`
	Tier       string `json:"tier,omitempty"`
}

// CustomerBillingPolicyAssignment names only the customer's explicit policy.
// A nil PolicyName means the customer inherits the ordinary tier/default policy.
type CustomerBillingPolicyAssignment struct {
	CustomerID string  `json:"customer_id"`
	PolicyName *string `json:"policy_name"`
}

// WastedSpendReport is one host-reported failed attempt that cost money.
// Source and SourceID are required and together form the idempotency key.
//
// Duplicate=true is served from a Redis claim, which is a cache: it expires with
// the widest configured wasted-spend window and does not survive a flush, so a
// replay after one is re-graded against grace and comes back Duplicate=false.
// The MONEY does not move twice either way — the direct-payer overage charge is
// keyed structurally in the usage ledger — and a replay with a changed Amount is
// refused rather than answered with the first result (or#891).
type WastedSpendReport struct {
	CustomerID  string `json:"customer_id"`
	Invoker     string `json:"invoker"`
	InvokerType string `json:"invoker_type,omitempty"`
	Currency    string `json:"currency,omitempty"`
	// Amount is the wasted spend in the currency's internal precision.
	Amount int64 `json:"amount,string"`
	// Source identifies the system reporting the waste (e.g. "inference-gateway").
	Source string `json:"source"`
	// SourceID is the idempotency key for this report within the Source namespace.
	SourceID string `json:"source_id"`
	Reason   string `json:"reason,omitempty"`
}

// UsageReport is one host-reported metered usage event (#797). CustomerID is
// the billed payer; Source+SourceID are REQUIRED and form the idempotency key
// within (merchant, payer, currency, event_type), enforced structurally by
// uq_usage_events_idem. Both halves must be REPRODUCIBLE across retries of the
// same event. A replay with the same Amount is accepted and neither re-records
// nor re-charges; a replay with a DIFFERENT Amount is refused (or#891) rather
// than answered with the first event. Amount is the host-priced cost in the currency's internal
// precision; 0 records a free/metered-only event (dimensions still aggregate
// through rate-card rating). OccurredAt (nil = now) places the event in its
// rating window — gauge segment reporters set it to segment end.
type UsageReport struct {
	CustomerID string           `json:"customer_id"`
	Invoker    string           `json:"invoker"`
	Currency   string           `json:"currency,omitempty"`
	EventType  string           `json:"event_type"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
	Amount     int64            `json:"amount,string"`
	Resource   string           `json:"resource,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
	Source     string           `json:"source"`
	SourceID   string           `json:"source_id"`
	// OccurredAt is the event time (nil = now).
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
}

// WastedSpendResponse reports how OpenRails handled a wasted-spend report.
type WastedSpendResponse struct {
	Currency             string `json:"currency"`
	PolicyCurrency       string `json:"policy_currency,omitempty"`
	RecordedAmount       int64  `json:"recorded_amount,string"`
	PolicyRecordedAmount int64  `json:"policy_recorded_amount,omitempty,string"`
	ForgivenAmount       int64  `json:"forgiven_amount,string"`
	PolicyForgivenAmount int64  `json:"policy_forgiven_amount,omitempty,string"`
	ChargedAmount        int64  `json:"charged_amount,string"`
	PolicyChargedAmount  int64  `json:"policy_charged_amount,omitempty,string"`
	Action               string `json:"action"`
	Duplicate            bool   `json:"duplicate,omitempty"`
}

// SpendLimitWindow is one fixed money-budget window in a hierarchical
// budget-scope policy (#473) — same shape as BudgetWindowInput
// (internal/service.SpendLimitWindowInput on the wire).
type SpendLimitWindow = BudgetWindowInput

// SpendDelegationInput is one payer-owned spend delegation. Machine clients use
// the merchant service surface; customers manage the same policy through their
// customer-owned treasury surface. Provenance (or#911) is the caller's opaque
// reference for what authorized the grant (e.g. a signed-document digest);
// stored on the grant and returned on reads, never interpreted by OpenRails.
type SpendDelegationInput struct {
	Scope      string             `json:"scope"`
	ScopeKey   string             `json:"scope_key,omitempty"`
	Windows    []SpendLimitWindow `json:"windows"`
	Provenance string             `json:"provenance,omitempty"`
}

// ResourceRevenueDailyRow is one day's revenue for a resource.
type ResourceRevenueDailyRow struct {
	Date     string `json:"date"`
	Currency string `json:"currency"`
	Amount   int64  `json:"amount,string"`
}

// ResourceRevenueResponse is the per-resource revenue rollup (#410).
type ResourceRevenueResponse struct {
	Currency      string                    `json:"currency"`
	RevenueAmount int64                     `json:"revenue_amount,string"`
	Daily         []ResourceRevenueDailyRow `json:"daily"`
}

// EntitlementRecord is one entitlement window. SourceID is the source
// resource's own wire id beside SourceType (see SourceRef): sub_… for
// subscription and grace sources, pay_… for one_off sources, the host's
// declared id for admin sources.
type EntitlementRecord struct {
	ID           string     `json:"id"`
	CustomerID   string     `json:"customer_id,omitzero"`
	Entitlement  string     `json:"entitlement"`
	StartAt      time.Time  `json:"start_at"`
	EndAt        *time.Time `json:"end_at,omitempty"`
	SourceID     *string    `json:"source_id,omitempty"`
	SourceType   string     `json:"source_type"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokeReason *string    `json:"revoke_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ProductAccessGrant is one active product-access row from the merchant lookup
// API. SourceID follows the EntitlementRecord rule (pay_… for purchase,
// sub_… for subscription, the declared id for admin).
type ProductAccessGrant struct {
	ID           string     `json:"id"`
	CustomerID   string     `json:"customer_id"`
	ProductID    string     `json:"product_id"`
	ProductKey   string     `json:"product_key,omitempty"`
	ProductName  string     `json:"product_name,omitempty"`
	SourceType   string     `json:"source_type"`
	SourceID     string     `json:"source_id,omitempty"`
	PaymentID    *string    `json:"payment_id,omitempty"`
	Status       string     `json:"status"`
	StartsAt     time.Time  `json:"starts_at"`
	EndsAt       *time.Time `json:"ends_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokeReason *string    `json:"revoke_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ProductAccessCheck is the response from a single product-access check.
type ProductAccessCheck struct {
	CustomerID string `json:"customer_id"`
	ProductID  string `json:"product_id"`
	ProductKey string `json:"product_key,omitempty"`
	HasAccess  bool   `json:"has_access"`
}

// AdmitBatchVerdict is one per-item verdict from POST /v1/merchant/admissions.
// Status is the HTTP-equivalent status the single Admit route would have
// returned for this item (200/402/403/429/4xx/5xx); Result is the full
// admission decision when one was reached.
type AdmitBatchVerdict struct {
	Status int            `json:"status"`
	Error  *ErrorDetails  `json:"error,omitempty"`
	Result *AdmitResponse `json:"result,omitempty"`
}

// Allowed reports whether this item has a live admission. Result.Allowed records
// the original decision even when a replay's state is expired or terminal.
func (v AdmitBatchVerdict) Allowed() bool {
	return v.Status == 200 && v.Result.Active()
}

// CreditLimitRequest carries an exact native-currency arrears limit.
type CreditLimitRequest struct {
	CustomerID        string `json:"customer_id"`
	Currency          string `json:"currency"`
	CreditLimitAmount int64  `json:"credit_limit_amount,string"`
}
