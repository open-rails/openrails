// Package openrails is the canonical OpenRails SDK surface (#338): ONE Go
// interface (Client) with two constructors —
//
//   - NewRemote(baseURL, opts...) talks to a standalone OpenRails over its
//     service-credential-authenticated /v1/merchant/* routes (this file + remote.go,
//     ported from the go-client module, which this package supersedes);
//   - openrails/embed.New(...).Client() runs the engine in-process and adapts
//     pkg/service.Service to the same interface.
//
// PARITY IS STRUCTURAL: the HTTP handlers are thin adapters over
// pkg/service.Service, the remote client is the inverse adapter, and the
// embedded client transcribes the handler mapping — so the two transports are
// the same code path with an optional wire round-trip inserted. The dual-mode
// conformance test in openrails/embed enforces this.
//
// This root package stays dependency-light: it MUST NOT import internal/* or
// pkg/embedded, so a remote-only consumer's binary does not link the engine
// (enforced by deps_test.go).
package openrails

import (
	"context"
	"time"

	"github.com/google/uuid"
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

// AdmissionClient is the Tensorhub hot path: batch admission, settle, release,
// wasted-spend reporting, and trust-tier read.
type AdmissionClient interface {
	AdmitBatch(ctx context.Context, items []AdmitRequest) ([]AdmitBatchVerdict, error)
	// Capture settles the admission/authorize hold request_id at the actual
	// amount, optionally recording a usage analytics event (#311/#410).
	Capture(ctx context.Context, requestID string, capturedAmount int64, usage *CaptureUsage) error
	// Release frees the admission/authorize hold request_id without charging.
	Release(ctx context.Context, requestID string) error
	// GetTier returns the payer's current trust tier (#477) for one currency:
	// the value OpenRails auto-maintains from same-currency cumulative paid spend
	// against the persisted schedule (#476), or a manual admin override. Empty
	// means the host treats it as the lowest/default trust tier.
	GetTier(ctx context.Context, customerID, currency string) (string, error)
	// ReportWastedSpend records host-reported WASTED $ (#497): delegated invokers
	// accrue toward their flat cutoff; direct payer credentials use trust-tier
	// grace and charge overage through the normal ledger. Source+SourceID are
	// required for retry idempotency.
	ReportWastedSpend(ctx context.Context, report WastedSpendReport) (*WastedSpendResponse, error)
}

// PolicySyncClient installs merchant-owned admission policy in one settings
// document.
type PolicySyncClient interface {
	GetMerchantSettings(ctx context.Context) (*MerchantSettings, error)
	SetMerchantSettings(ctx context.Context, settings MerchantSettings) error
	SetCustomerSpendDelegations(ctx context.Context, customerID string, delegations []SpendDelegationInput) error
}

// AdminFundingClient is the small non-hot-path funding/reporting surface used
// by standalone admin jobs.
type AdminFundingClient interface {
	// DepositCredits mints a credit block for a payer (admin funding, promotions,
	// money-in settlement). Returns the ledger transaction created.
	DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error)
	// SetCreditLimit sets the admin-managed arrears credit line for a payer in one
	// currency. A zero limit removes the credit line.
	SetCreditLimit(ctx context.Context, customerID, currency string, creditLimit int64) error
	// UsageRollup returns grouped spend aggregates for a payer and currency over
	// [from, to]. groupBy selects the aggregation dimension (e.g. "resource", "invoker").
	UsageRollup(ctx context.Context, customerID, currency string, from, to time.Time, groupBy string) ([]UsageRollupRow, error)
	// ResourceRevenueDaily returns per-day revenue for a resource across all
	// payers in the merchant (#410).
	ResourceRevenueDaily(ctx context.Context, resource, currency string, fromUnix, toUnix int64) (*ResourceRevenueResponse, error)
}

// CustomerLookupClient reads customer-forward billing state.
type CustomerLookupClient interface {
	// Balance returns the payer's balance snapshot.
	Balance(ctx context.Context, customerID string) (*BalanceResponse, error)
	// GetCreditAccount reads a payer's balance snapshot for one currency.
	GetCreditAccount(ctx context.Context, customerID, currency string) (*CreditAccount, error)
	// ListActiveEntitlements returns the entitlement records active at `at`
	// for subjects addressed by their EXTERNAL identity — the subject ids the
	// host's auth system already holds, scoped to the request credential's
	// merchant (#555: no issuer; identity is (merchant, subject)). Always batch (#354):
	// one engine query answers the whole list, keyed by subject with an entry
	// per requested subject after trim + dedupe; an unknown subject — a user
	// who has never touched billing — is an empty slice, never an error.
	// Single lookup = an array of one. Max 500 subjects per call; over-cap
	// errors, never silently truncates. A zero `at` means "now". For
	// token-issuance enrichment and list renders: bake names into token
	// claims and gate per-request from the token, not from this call.
	ListActiveEntitlements(ctx context.Context, subjects []string, at time.Time) (map[string][]EntitlementRecord, error)
	// ListEntitlements is the single-subject form of ListActiveEntitlements.
	ListEntitlements(ctx context.Context, subject string, at time.Time) ([]EntitlementRecord, error)
	// HasEntitlement checks one entitlement for one subject at `at`.
	HasEntitlement(ctx context.Context, subject, entitlement string, at time.Time) (bool, error)
	// ListCustomersWithEntitlement is the REVERSE of ListActiveEntitlements (#535):
	// the customer ids (== external subject ids, #364 UUID-only) holding an ACTIVE
	// window of `entitlement` for the merchant. It walks the keyset-paginated
	// reverse query to completion and returns the full set. A zero `at` means
	// "now". Backs a host directory's filter-by-entitlement (the authkit
	// EntitlementFilterProvider).
	ListCustomersWithEntitlement(ctx context.Context, entitlement string, at time.Time) ([]string, error)
	// ListProductAccess lists active product-access grants for one subject.
	ListProductAccess(ctx context.Context, subject string) ([]ProductAccessGrant, error)
	// HasProductAccess checks one product id for one subject.
	HasProductAccess(ctx context.Context, subject, productID string) (bool, error)
}

// Client is the complete SDK surface. New hot-path callers should depend on the
// smaller interfaces above.
type Client interface {
	AdmissionClient
	PolicySyncClient
	AdminFundingClient
	CustomerLookupClient
}

// SelfIssuer is the issuer keying customers rows for self-service
// identities whose subject is the user's own UUID — what an embedded host
// passes to ListActiveEntitlements for its own users (internal/db/repo
// EnsureCustomerID materializes rows under it).
const SelfIssuer = "openrails:self"

// CustomerID is the OpenRails customer UUID a charge is billed to.
type CustomerID uuid.UUID

func (id CustomerID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id CustomerID) String() string  { return uuid.UUID(id).String() }
func (id CustomerID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }

// DepositCreditsRequest mints a credit block for a payer (admin funding,
// promotions, money-in settlement). Amount is in the currency's internal
// precision.
type DepositCreditsRequest struct {
	CustomerID *CustomerID
	Invoker    string
	Currency   string
	// Amount is the deposit size in the currency's internal precision (e.g. cents for USD).
	Amount int64
	// Source identifies the system of record for this deposit (e.g. "stripe", "manual").
	Source string
	// SourceID is the idempotency key for the deposit within the Source namespace.
	// Duplicate (Source, SourceID) pairs are rejected, preventing double-deposits.
	SourceID    *uuid.UUID
	ExpiresAt   *time.Time
	Description string
}

// CreditTransaction is the ledger row returned by capture/withdraw/deposit.
// Field names match the wire exactly (the handler serializes
// pkg/service.CreditTransaction with Go field names, no json tags). The
// customer field is CustomerID, matching the service struct's Go field name.
type CreditTransaction struct {
	ID              uuid.UUID
	CustomerID      uuid.UUID
	Invoker         string
	Currency        string
	Amount          int64
	BalanceAfter    *int64
	TransactionType string
	Status          string
	Authorized      *int64
	Captured        *int64
	Source          string
	SourceID        *string
	ExpiresAt       *time.Time
	Description     *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AdmitRequest is one item in POST /v1/merchant/admissions. It checks payer money
// capacity, delegated spend policy, delegated wasted-spend cutoff, and places the
// request hold when allowed.
//
// TrustTier selects money policy. Resource is host-side attribution only;
// endpoint authorization stays with the host.
// EstimatedAmount is the upper-bound charge to hold. A zero EstimatedAmount runs
// the limit checks without placing a money hold.
type AdmitRequest struct {
	CustomerID  string `json:"customer_id"`
	Invoker     string `json:"invoker"`
	InvokerType string `json:"invoker_type,omitempty"`
	TrustTier   string `json:"trust_tier,omitempty"`
	// Tier is a deprecated alias for TrustTier, kept for current Go callers.
	Tier            string `json:"tier,omitempty"`
	Resource        string `json:"resource,omitempty"`
	Currency        string `json:"currency,omitempty"`
	EstimatedAmount int64  `json:"estimated_amount"`
	RequestID       string `json:"request_id"`
	Source          string `json:"source,omitempty"`
	ExpiresAt       *int64 `json:"expires_at,omitempty"`
	// Roles are the immutable role UUIDs the invoker holds (#473). Each role with a
	// matching (subject, role) budget-scope policy gates this request's spend in
	// the same admit verdict. The host reads them from the delegated
	// JWT/permission set. Empty = no role-scoped budget applies.
	Roles []uuid.UUID `json:"roles,omitempty"`
}

// AdmitResponse is the admission verdict (pkg/service.AdmitResult on the wire).
// Allowed=false carries a BlockedBy axis ("budget" | "abuse" | "money") and a
// DenyCode when available. A successful money-bearing admit creates a request_id
// keyed Redis hold; BudgetReservationID is the budget reservation settled with
// the request. A deny is returned as (Allowed=false, nil error) on both
// transports even though HTTP maps it to 402/403/429.
type AdmitResponse struct {
	Allowed             bool           `json:"allowed"`
	BlockedBy           string         `json:"blocked_by,omitempty"`
	DenyCode            string         `json:"deny_code,omitempty"`
	Currency            string         `json:"currency,omitempty"`
	EstimatedAmount     int64          `json:"estimated_amount,omitempty"`
	PolicyCurrency      string         `json:"policy_currency,omitempty"`
	PolicyAmount        int64          `json:"policy_amount,omitempty"`
	StartCapacityAmount int64          `json:"start_capacity_amount,omitempty"`
	RetryAfterSeconds   int64          `json:"retry_after_seconds,omitempty"`
	HoldExpiresAt       *time.Time     `json:"hold_expires_at,omitempty"`
	BudgetReservationID string         `json:"budget_reservation_id,omitempty"`
	BudgetWindows       []BudgetWindow `json:"budget_windows,omitempty"`
}

// CaptureUsage carries the analytics dimensions recorded alongside a capture so
// OpenRails can serve per-resource/function/tier/invoker spend (#410). Nil = no
// usage event (a plain capture).
type CaptureUsage struct {
	// EventType classifies the usage event (e.g. "inference", "storage"). Required
	// for the event to be recorded; a blank EventType suppresses the usage event.
	EventType string
	// Resource is the host-defined resource attribution key (e.g. model name, endpoint).
	Resource string
	// Metadata holds arbitrary key/value dimensions for analytics rollups.
	Metadata map[string]any
	// Source identifies the system that generated this usage event.
	Source string
	// SourceID is the idempotency key for this usage event within the Source namespace.
	SourceID string
}

// BalanceResponse is the GET /v1/merchant/credits/balance snapshot (handler
// serviceBalanceResponse). NOTE: the wire field for the owed amount is
// outstanding_owed_amount.
type BalanceResponse struct {
	Currency              string `json:"currency"`
	BillingMode           string `json:"billing_mode"`
	BalanceAmount         int64  `json:"balance_amount"`
	HeldAmount            int64  `json:"held_amount"`
	AvailableAmount       int64  `json:"available_amount"`
	OutstandingOwedAmount int64  `json:"outstanding_owed_amount"`
}

// CreditAccount is the OpenRails service balance/policy snapshot for one
// customer + currency pair. All amounts are in the currency's internal
// precision (e.g. cents for USD).
type CreditAccount struct {
	CustomerID  string `json:"customer_id"`
	Currency    string `json:"currency"`
	BillingMode string `json:"billing_mode"`
	// BalanceAmount is the total prepaid credit balance (excluding holds).
	BalanceAmount int64 `json:"balance_amount"`
	// HeldAmount is the sum of outstanding authorization holds not yet captured or released.
	HeldAmount int64 `json:"held_amount"`
	// AvailableAmount is BalanceAmount minus HeldAmount — the credit available for new admits.
	AvailableAmount int64 `json:"available_amount"`
	// OutstandingOwedAmount is the unpaid postpaid balance (postpaid billing mode only).
	OutstandingOwedAmount int64 `json:"outstanding_owed_amount"`
}

// AccountSettingsInput patches an OpenRails credit account policy.
// All pointer fields are optional — nil means "no change" for that setting.
// Amounts are in the currency's internal precision (e.g. cents for USD).
type AccountSettingsInput struct {
	// BillingMode controls how the account is charged ("prepaid" or "postpaid").
	BillingMode *string `json:"billing_mode,omitempty"`
	// MaxSpendPerDay is the rolling-24h spend ceiling in currency-internal units.
	MaxSpendPerDay *int64 `json:"max_spend_per_day,omitempty"`
	// MaxSpendPerMonth is the calendar-month spend ceiling in currency-internal units.
	MaxSpendPerMonth *int64 `json:"max_spend_per_month,omitempty"`
	// MaxOutstandingOwedAmount caps outstanding postpaid balance in currency-internal units.
	MaxOutstandingOwedAmount *int64 `json:"max_outstanding_owed_amount,omitempty"`
	// LowBalanceThreshold triggers a low-balance alert when prepaid balance falls below this.
	LowBalanceThreshold *int64 `json:"low_balance_threshold,omitempty"`
	AutoTopupEnabled    *bool  `json:"auto_topup_enabled,omitempty"`
	// AutoTopupAmountCents is the topup deposit size in currency-internal units.
	AutoTopupAmountCents   *int64  `json:"auto_topup_amount_cents,omitempty"`
	AutoTopupPaymentMethod *string `json:"auto_topup_payment_method_id,omitempty"`
	// DefaultCreditExpiryDays is the default lifetime in days for credit deposits that
	// don't carry an explicit ExpiresAt.
	DefaultCreditExpiryDays *int `json:"default_credit_expiry_days,omitempty"`
	// HardStopOnBreach rejects new admissions when any spend limit is breached rather
	// than allowing with a warning.
	HardStopOnBreach *bool `json:"hard_stop_on_breach,omitempty"`
	// AlertThresholdPct triggers an alert when the balance falls below this percentage
	// of the low-balance threshold (0–100).
	AlertThresholdPct *int `json:"alert_threshold_pct,omitempty"`
}

// UsageRollupRow is one grouped spend bucket from OpenRails.
type UsageRollupRow struct {
	Key         string `json:"key"`
	Currency    string `json:"currency"`
	EventCount  int64  `json:"event_count"`
	TotalAmount int64  `json:"total_amount"`
}

// BudgetWindowInput is a caller-supplied fixed budget window sent to
// OpenRails. The host owns the policy; OpenRails owns the spend actuals.
type BudgetWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}

// MerchantProfileInput is public/communication metadata stored per merchant.
type MerchantProfileInput struct {
	DisplayName string `json:"display_name,omitempty"`
	LogoURL     string `json:"logo_url,omitempty"`
	FromEmail   string `json:"from_email,omitempty"`
	SupportURL  string `json:"support_url,omitempty"`
}

// MerchantSettings is the merchant-owned admission/policy document installed by
// standalone policy sync jobs.
type MerchantSettings struct {
	Profile                           *MerchantProfileInput  `json:"profile,omitempty"`
	TierSchedules                     []MerchantTierSchedule `json:"tier_schedules,omitempty"`
	TierSpendLimits                   []PayerSpendLimitInput `json:"tier_spend_limits,omitempty"`
	DelegatedInvokerWastedSpendLimits []BudgetWindowInput    `json:"delegated_invoker_wasted_spend_limits,omitempty"`
}

// MerchantTierSchedule is one currency's trust-tier ladder.
type MerchantTierSchedule struct {
	Currency string             `json:"currency"`
	Schedule []TierScheduleRung `json:"schedule"`
}

// BudgetWindow is one computed window from admission policy checks and Admit's
// budget_windows
// (pkg/service.AdmitBudgetWindowDTO on the wire).
type BudgetWindow struct {
	Key               string `json:"key"`
	Currency          string `json:"currency"`
	Limit             int64  `json:"limit"`
	Used              int64  `json:"used"`
	Reserved          int64  `json:"reserved"`
	Remaining         int64  `json:"remaining"`
	ResetAfterSeconds int64  `json:"reset_after_seconds"`
	// ResetAt is the exact window boundary (#337 fixed windows).
	ResetAt time.Time `json:"reset_at,omitzero"`
	Allowed bool      `json:"allowed"`
}

// PayerSpendLimitInput configures a per-payer trust-tier policy via merchant
// settings (#298): fixed money-budget windows and wasted
// spend grace windows (pkg/service.PayerSpendLimitInput on the wire; customer_id
// travels alongside it).
type PayerSpendLimitInput struct {
	TrustTier string `json:"trust_tier,omitempty"`
	// Tier is a deprecated alias for TrustTier, kept for current Go callers.
	Tier           string              `json:"tier,omitempty"`
	BudgetWindows  []BudgetWindowInput `json:"budget_windows"`
	PolicyCurrency string              `json:"policy_currency,omitempty"`
	// BadSpendWindows are the #497 per-PAYER direct-credential wasted-spend grace
	// windows for this trust tier: at most Limit of host-reported wasted spend is
	// forgiven per window; direct-payer overage is charged.
	BadSpendWindows []BudgetWindowInput `json:"bad_spend_windows,omitempty"`
}

// AbuseUsageWindow is one wasted-spend window's running $ total (#488).
type AbuseUsageWindow struct {
	Key        string `json:"key"`
	Currency   string `json:"currency"`
	Window     string `json:"window"`
	Used       int64  `json:"used"`
	Limit      int64  `json:"limit"`
	OverBudget bool   `json:"over_budget"`
}

// AbuseUsageResponse is the running wasted-$ totals for a payer + invoker (#488).
type AbuseUsageResponse struct {
	Currency       string             `json:"currency"`
	PayerWindows   []AbuseUsageWindow `json:"payer_windows"`
	InvokerWindows []AbuseUsageWindow `json:"invoker_windows,omitempty"`
}

// WastedSpendReport is one host-reported failed attempt that cost money.
// Source and SourceID are required and together form the idempotency key —
// duplicate (Source, SourceID) reports are accepted but recorded as Duplicate=true.
type WastedSpendReport struct {
	CustomerID  string `json:"customer_id"`
	Invoker     string `json:"invoker"`
	InvokerType string `json:"invoker_type,omitempty"`
	Currency    string `json:"currency,omitempty"`
	// Amount is the wasted spend in the currency's internal precision.
	Amount int64 `json:"amount"`
	// Source identifies the system reporting the waste (e.g. "inference-gateway").
	Source string `json:"source"`
	// SourceID is the idempotency key for this report within the Source namespace.
	SourceID string `json:"source_id"`
	Reason   string `json:"reason,omitempty"`
}

// WastedSpendResponse reports how OpenRails handled a wasted-spend report.
type WastedSpendResponse struct {
	Currency             string `json:"currency"`
	PolicyCurrency       string `json:"policy_currency,omitempty"`
	RecordedAmount       int64  `json:"recorded_amount"`
	PolicyRecordedAmount int64  `json:"policy_recorded_amount,omitempty"`
	ForgivenAmount       int64  `json:"forgiven_amount"`
	PolicyForgivenAmount int64  `json:"policy_forgiven_amount,omitempty"`
	ChargedAmount        int64  `json:"charged_amount"`
	PolicyChargedAmount  int64  `json:"policy_charged_amount,omitempty"`
	Action               string `json:"action"`
	Duplicate            bool   `json:"duplicate,omitempty"`
}

// TierScheduleRung is one rung of the persisted same-currency tier ladder set
// via merchant settings (#476): a payer reaches Tier once its
// cumulative paid spend in the schedule currency is at least
// MinCumulativePaidAmount. Order ascending by MinCumulativePaidAmount (the
// server sorts defensively regardless).
type TierScheduleRung struct {
	Tier                    string `json:"tier"`
	MinCumulativePaidAmount int64  `json:"min_cumulative_paid_amount"`
}

// SpendLimitWindow is one fixed money-budget window in a hierarchical
// budget-scope policy (#473) — same shape as BudgetWindowInput
// (pkg/service.SpendLimitWindowInput on the wire).
type SpendLimitWindow struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}

// SpendDelegationInput is one payer-owned delegation in
// /v1/customers/:customer_id/spend-delegations.
type SpendDelegationInput struct {
	Scope    string             `json:"scope"`
	ScopeKey string             `json:"scope_key,omitempty"`
	RoleID   string             `json:"role_id,omitempty"`
	Windows  []SpendLimitWindow `json:"windows"`
}

// ResourceRevenueDailyRow is one day's revenue for a resource.
type ResourceRevenueDailyRow struct {
	Date     string `json:"date"`
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"`
}

// ResourceRevenueResponse is the per-resource revenue rollup (#410).
type ResourceRevenueResponse struct {
	Currency      string                    `json:"currency"`
	RevenueAmount int64                     `json:"revenue_amount"`
	Daily         []ResourceRevenueDailyRow `json:"daily"`
}

// EntitlementRecord is one active entitlement row — the handler's
// ServiceEntitlementRecord wire shape (entitlements.go) verbatim.
type EntitlementRecord struct {
	ID           string     `json:"id"`
	CustomerID   string     `json:"customer_id,omitempty"`
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
// API.
type ProductAccessGrant struct {
	ID           string     `json:"id"`
	CustomerID   string     `json:"customer_id"`
	ProductID    string     `json:"product_id"`
	ProductSlug  string     `json:"product_slug,omitempty"`
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
	HasAccess  bool   `json:"has_access"`
}

// AdmitBatchVerdict is one per-item verdict from POST /v1/merchant/admissions.
// Status is the HTTP-equivalent status the single Admit route would have
// returned for this item (200/402/403/429/4xx/5xx); Result is the full
// admission decision when one was reached.
type AdmitBatchVerdict struct {
	Status int            `json:"status"`
	Error  string         `json:"error,omitempty"`
	Result *AdmitResponse `json:"result,omitempty"`
}

// Allowed reports whether this item was admitted.
func (v AdmitBatchVerdict) Allowed() bool {
	return v.Status == 200 && v.Result != nil && v.Result.Allowed
}
