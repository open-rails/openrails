// Package openrails is the canonical OpenRails SDK surface (#338): ONE Go
// interface (Client) with two constructors —
//
//   - NewRemote(baseURL, opts...) talks to a standalone OpenRails over its
//     service-token-authenticated /v1/service/* routes (this file + remote.go,
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
	"encoding/json"
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

// Client is the unified OpenRails service surface (#338): credits/holds,
// unified admission, budgets, tier policy, account settings, and usage/revenue
// analytics. All types are wire types (strings/ints/time) — no internal types.
//
// Method semantics (idempotency keys, deny-vs-error, defaulting) are defined by
// the standalone HTTP handlers in internal/http/handlers/service_*.go; both
// implementations follow them exactly.
type Client interface {
	// Admit performs the unified admission check + atomic hold (#298/#403/#404):
	// throughput + budget + suspension/blocklist/resource gating + credit hold in
	// one verdict. Idempotent on req.RequestID. A clean deny is (Allowed=false,
	// nil error) on BOTH transports.
	Admit(ctx context.Context, req AdmitRequest) (*AdmitResponse, error)
	// Authorize is the atomic authorize+hold (#235/#247), idempotent on
	// req.RequestID. A clean deny is (Allowed=false, nil error).
	Authorize(ctx context.Context, req AuthorizeRequest) (*AuthorizeResponse, error)
	// HoldCredits reserves credits for an estimate (legacy hold route).
	HoldCredits(ctx context.Context, req HoldCreditsRequest) (*CreditHold, error)
	// CaptureHold settles a hold and returns the ledger transaction.
	CaptureHold(ctx context.Context, req CaptureHoldRequest) (*CreditTransaction, error)
	// ReleaseHold frees a hold without charging.
	ReleaseHold(ctx context.Context, holdID uuid.UUID) error
	// WithdrawCredits debits credits directly.
	WithdrawCredits(ctx context.Context, req WithdrawCreditsRequest) (*CreditTransaction, error)
	// DepositCredits mints a credit block for a payer.
	DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error)
	// Capture settles the admission/authorize hold reservationID at the actual
	// amount, optionally recording a usage analytics event (#311/#410).
	Capture(ctx context.Context, reservationID string, capturedAmount int64, usage *CaptureUsage) error
	// Release frees the admission/authorize hold reservationID without charging.
	Release(ctx context.Context, reservationID string) error
	// Balance returns the payer's balance snapshot.
	Balance(ctx context.Context, customerID string) (*BalanceResponse, error)
	// GetCreditAccount reads a payer's balance + policy snapshot. currency is
	// optional (empty defaults to "USD", #476).
	GetCreditAccount(ctx context.Context, customerID, currency string) (*CreditAccount, error)
	// SetCreditAccountSettings upserts a payer's billing account settings and
	// returns the refreshed account snapshot. currency is optional (empty
	// defaults to "USD", #476).
	SetCreditAccountSettings(ctx context.Context, customerID, currency string, in AccountSettingsInput) (*CreditAccount, error)
	// ListCreditTransactions reads a payer's transaction history and passes the
	// canonical JSON through ({"transactions":[...],"total":N}). currency is
	// optional (empty defaults to "USD", #476).
	ListCreditTransactions(ctx context.Context, customerID, currency string, limit int) (json.RawMessage, error)
	// UsageRollup returns grouped spend over [from, to). groupBy is
	// resource|invoker|function|tier.
	UsageRollup(ctx context.Context, customerID, currency string, from, to time.Time, groupBy string) ([]UsageRollupRow, error)
	// BudgetCheck computes fixed money-budget windows for (payer, invoker) against
	// CALLER-SUPPLIED windows without reserving (#410, #337 cadence).
	BudgetCheck(ctx context.Context, tenantSubjectID, invokerID, currency string, windows []BudgetWindowInput, requestedAmount int64) ([]BudgetWindow, error)
	// BudgetStatus returns the invoker's fixed money-budget windows under the
	// payer's stored tier policy without reserving (#304 introspection).
	BudgetStatus(ctx context.Context, tenantSubjectID, invokerID, currency, tier string) ([]BudgetWindow, error)
	// SetTierPolicy upserts a per-payer tier policy (#298): throughput windows +
	// entitled resources + fixed money-budget windows.
	SetTierPolicy(ctx context.Context, tenantSubjectID string, in TierPolicyInput) error
	// SetTierSchedule persists the tenant's tier SCHEDULE once (#476): the ordered
	// ladder [{tier, min_cumulative_paid_amount}]. OpenRails then AUTO-maintains
	// each payer's tier from cumulative spend — the host never cranks graduation.
	// An EMPTY tenantSubjectID sets the tenant-wide default schedule (the common
	// case); a non-empty one sets a per-subject override. owner=platform (the
	// subject cannot see/loosen it).
	SetTierSchedule(ctx context.Context, tenantSubjectID string, schedule []TierScheduleRung) error
	// SetInvokerWastedSpendPolicy configures the operator-set FLAT per-INVOKER
	// wasted-spend policy (#496): the fixed delegated-user backstop OpenRails
	// enforces at admit. It is merchant-scoped and does not vary by payer/customer.
	// When unset, OpenRails falls back to its hardcoded default. Reuses
	// BudgetWindowInput (key + window_seconds + limit; cadence ignored — these are
	// flat rolling windows). OPERATOR-only — NOT self-serve.
	SetInvokerWastedSpendPolicy(ctx context.Context, windows []BudgetWindowInput) error
	// GetTier returns the payer's CURRENT graduated tier (#477): the tier
	// OpenRails auto-maintains from cumulative paid spend against the persisted
	// schedule (#476), or a manual admin override. Empty when the payer has never
	// graduated (the host treats that as the lowest/default tier). The read a host
	// uses to drive its OWN per-tier capacity (e.g. tensorhub's scheduler in-flight
	// concurrency cap) without re-deriving the ladder or cranking graduation.
	GetTier(ctx context.Context, tenantSubjectID string) (string, error)
	// ReportWastedSpend records host-reported WASTED $ (#497): delegated invokers
	// accrue toward their flat cutoff; direct payer credentials use trust-tier
	// grace and charge overage through the normal ledger. Source+SourceID are
	// required for retry idempotency.
	ReportWastedSpend(ctx context.Context, report WastedSpendReport) (*WastedSpendResponse, error)
	// AbuseUsage returns the payer's and invoker's running WASTED-$ totals + budgets
	// (#488 introspection) in one currency. Empty invoker omits the invoker
	// windows; empty tier resolves the payer's current tier.
	AbuseUsage(ctx context.Context, tenantSubjectID, invoker, currency, tier string) (*AbuseUsageResponse, error)
	// SetCreditLimit sets the admin/operator arrears credit line for a payer
	// (#489): under billing_mode=arrears the balance may go NEGATIVE up to
	// creditLimit; AdmitHold denies insufficient_credit when a new hold would
	// exceed it. 0 = off (prepaid / existing-arrears behavior, unchanged).
	// OPERATOR-only — NOT self-serve.
	SetCreditLimit(ctx context.Context, tenantSubjectID, currency string, creditLimit int64) error
	// GetCreditLimit returns the admin-set arrears credit line for a payer (#489).
	GetCreditLimit(ctx context.Context, tenantSubjectID, currency string) (int64, error)
	// SetSubjectBudgetPolicy upserts a SUBJECT-owned hierarchical budget-scope
	// policy (#473): the subject's self cap (scope=subject) or a (subject, role)
	// pool (scope=role, RoleID = the role uuid). The owner is forced to "subject"
	// — this path can never write or loosen a platform-owned cap.
	SetSubjectBudgetPolicy(ctx context.Context, tenantSubjectID string, in SubjectBudgetPolicyInput) error
	// SetPlatformBudgetPolicy upserts a PLATFORM-owned budget cap (#473): the
	// platform->payer cap (scope=subject). Owner is forced to "platform".
	// Platform-admin-authorized on the HTTP transport; a direct call when embedded.
	SetPlatformBudgetPolicy(ctx context.Context, tenantSubjectID string, in PlatformBudgetPolicyInput) error
	// SubjectBudgetPolicies reads back ONLY the subject-owned budget-scope policies
	// (#473) for the host to reconcile — platform-owned caps are deliberately
	// invisible here.
	SubjectBudgetPolicies(ctx context.Context, tenantSubjectID string) ([]SubjectBudgetPolicy, error)
	// ResourceRevenueDaily returns per-day revenue for a resource across all
	// payers in the tenant (#410).
	ResourceRevenueDaily(ctx context.Context, resource, currency string, fromUnix, toUnix int64) (*ResourceRevenueResponse, error)
	// ListActiveEntitlements returns the entitlement records active at `at`
	// for subjects addressed by their EXTERNAL identity — the issuer +
	// subject ids the host's auth system already holds (the same key a
	// delegated token pins via `iss`/`delegated_sub`). Always batch (#354):
	// one engine query answers the whole list, keyed by subject with an entry
	// per requested subject after trim + dedupe; an unknown subject — a user
	// who has never touched billing — is an empty slice, never an error.
	// Single lookup = an array of one. Max 500 subjects per call; over-cap
	// errors, never silently truncates. A zero `at` means "now". For
	// token-issuance enrichment and list renders: bake names into token
	// claims and gate per-request from the token, not from this call.
	ListActiveEntitlements(ctx context.Context, issuer string, subjects []string, at time.Time) (map[string][]EntitlementRecord, error)
	// OpenWindow opens a prepaid credit window (#335): a REAL hold — the funds
	// leave the payer's available balance now. ErrInsufficientCredits on a payer
	// who can't cover it.
	OpenWindow(ctx context.Context, req OpenWindowRequest) (*CreditWindow, error)
	// SettleWindowItems flushes a cross-payer batch of settled actuals. The call
	// succeeds with per-item results — one item's failure never fails the flush —
	// and is safe to re-send wholesale (per-item idempotency on RequestID).
	SettleWindowItems(ctx context.Context, items []WindowSettleItem) ([]WindowSettleResult, error)
	// RefillWindow extends an open window: amount > 0 reserves more funds
	// (ErrInsufficientCredits applies exactly like open), ttlSeconds > 0 pushes
	// expires_at out. At least one must be positive.
	RefillWindow(ctx context.Context, windowID uuid.UUID, amount, ttlSeconds int64) (*CreditWindow, error)
	// CloseWindow releases the window's unsettled remainder back to the payer's
	// available balance. Idempotent: re-closing returns the closed window.
	CloseWindow(ctx context.Context, windowID uuid.UUID) (*CreditWindow, error)
	// AdmitBatch performs one cross-payer batch admission (#335): N admit items
	// (mixed payers) in ONE call with per-item verdicts. Per-item isolation: one
	// item's deny or error never fails the batch; the returned slice is
	// positionally aligned with items. Idempotent per item on RequestID like Admit.
	AdmitBatch(ctx context.Context, items []AdmitRequest) ([]AdmitBatchVerdict, error)
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

// AuthorizeRequest is the body for POST /v1/service/credits/authorize. It is the
// atomic authorize+hold: OpenRails checks the payer's balance and, if allowed,
// places a hold for EstimatedAmount in a single idempotent call keyed on RequestID.
type AuthorizeRequest struct {
	CustomerID string `json:"customer_id"`
	Invoker    string `json:"invoker"`
	// Currency is the ledger unit; empty defaults to "USD" (#476). A free string
	// so #475 qualified custom-credit unit codes ride the same field.
	Currency        string `json:"currency,omitempty"`
	EstimatedAmount int64  `json:"estimated_amount"`
	RequestID       string `json:"request_id"`
	// ExpiresAt (unix seconds) bounds the placed hold; nil applies the engine's
	// default hold TTL.
	ExpiresAt *int64 `json:"expires_at,omitempty"`
}

// AuthorizeResponse is the OpenRails decision (handler serviceAuthorizeResponse).
// Allowed=false with a DenyCode is a definitive deny; ReservationID is the hold
// handle to later capture or release.
type AuthorizeResponse struct {
	Allowed               bool   `json:"allowed"`
	DenyCode              string `json:"deny_code,omitempty"`
	BillingMode           string `json:"billing_mode,omitempty"`
	Currency              string `json:"currency"`
	AvailableAmount       int64  `json:"available_amount"`
	OutstandingOwedAmount int64  `json:"outstanding_owed_amount"`
	RemainingTodayAmount  int64  `json:"remaining_today_amount,omitempty"`
	RetryAfterSeconds     int64  `json:"retry_after_seconds,omitempty"`
	ReservationID         string `json:"reservation_id,omitempty"`
}

// HoldCreditsRequest reserves credits for an estimate.
type HoldCreditsRequest struct {
	CustomerID *CustomerID
	Invoker    string
	// Currency is the ledger unit; empty defaults to "USD" (#476).
	Currency  string
	Amount    int64
	Source    string
	SourceID  string
	ExpiresAt time.Time
}

// CreditHold is the reservation returned by a successful hold. Field names
// match the wire exactly: the handler serializes pkg/service.CreditHold with Go
// field names (no json tags).
type CreditHold struct {
	ID        uuid.UUID
	Invoker   string
	Currency  string
	Amount    int64
	Source    string
	SourceID  string
	Status    string
	ExpiresAt time.Time
	Captured  *int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CaptureHoldRequest settles a hold at the actual amount.
type CaptureHoldRequest struct {
	HoldID uuid.UUID
	Amount int64
}

// WithdrawCreditsRequest debits credits directly.
type WithdrawCreditsRequest struct {
	CustomerID *CustomerID
	Invoker    string
	// Currency is the ledger unit; empty defaults to "USD" (#476).
	Currency string
	Amount   int64
	Source   string
	SourceID *uuid.UUID
}

// DepositCreditsRequest mints a credit block for a payer (admin funding,
// promotions, money-in settlement). Amount is in the currency's internal
// precision.
type DepositCreditsRequest struct {
	CustomerID *CustomerID
	Invoker    string
	// Currency is the ledger unit; empty defaults to "USD" (#476).
	Currency    string
	Amount      int64
	Source      string
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

// AdmitRequest is the body for POST /v1/service/admit — the UNIFIED admission
// verdict (#403/#404). It folds rate-limit/quota/concurrency (throughput),
// money-budget windows, suspension/blocklist/resource gating, AND the credit
// hold into ONE call, returning ONE reservation for later capture/release.
//
// Tier/Resource select the throughput + budget policy + resource gating;
// Amounts is the per-request throughput consumption (e.g. {"request":1}).
// EstimatedAmount is the upper-bound charge to hold. A zero EstimatedAmount runs
// the limit checks without placing a money hold.
type AdmitRequest struct {
	CustomerID      string           `json:"customer_id"`
	Invoker         string           `json:"invoker"`
	InvokerType     string           `json:"invoker_type,omitempty"`
	Tier            string           `json:"tier,omitempty"`
	Resource        string           `json:"resource,omitempty"`
	Amounts         map[string]int64 `json:"amounts,omitempty"`
	Currency        string           `json:"currency,omitempty"`
	EstimatedAmount int64            `json:"estimated_amount"`
	RequestID       string           `json:"request_id"`
	Source          string           `json:"source,omitempty"`
	ExpiresAt       *int64           `json:"expires_at,omitempty"`
	// Roles are the immutable role UUIDs the invoker holds (#473). Each role with a
	// matching (subject, role) budget-scope policy gates this request's spend in
	// the same admit verdict. The host reads them from the delegated
	// JWT/permission set. Empty = no role-scoped budget applies.
	Roles []uuid.UUID `json:"roles,omitempty"`
	// TenantThroughput is the per-TENANT fixed-window throughput policy OpenRails
	// should enforce on this invoke (#404).
	TenantThroughput []AdmitThroughputWindow `json:"tenant_throughput,omitempty"`
}

// AdmitThroughputWindow is one tenant fixed-window throughput limit (#404):
// at most Max units of Unit per WindowSeconds (Unit defaults to "request").
type AdmitThroughputWindow struct {
	Unit          string `json:"unit,omitempty"`
	WindowSeconds int64  `json:"window_seconds"`
	Max           int64  `json:"max"`
}

// AdmitWindow is one throughput window's state (mirrors the x-ratelimit-*
// headers; pkg/service.AdmitWindowDTO on the wire).
type AdmitWindow struct {
	Unit              string `json:"unit"`
	Limit             int64  `json:"limit"`
	Remaining         int64  `json:"remaining"`
	ResetAfterSeconds int64  `json:"reset_after_seconds"`
}

// AdmitResponse is the unified verdict (pkg/service.AdmitResult on the wire).
// Allowed=false carries a BlockedBy axis ("throughput" | "money" | "budget" |
// "blocked" | "suspended" | "unverified" | "resource") and, on a money deny, a
// DenyCode. ReservationID is the placed credit hold to capture/release;
// BudgetReservationID is the budget reservation (settled implicitly with the
// hold). A deny is returned as (Allowed=false, nil error) on BOTH transports
// even though the HTTP layer carries it as 402/403/429.
type AdmitResponse struct {
	Allowed             bool           `json:"allowed"`
	BlockedBy           string         `json:"blocked_by,omitempty"`
	BlockedUnit         string         `json:"blocked_unit,omitempty"`
	DenyCode            string         `json:"deny_code,omitempty"`
	Currency            string         `json:"currency,omitempty"`
	RetryAfterSeconds   int64          `json:"retry_after_seconds,omitempty"`
	Windows             []AdmitWindow  `json:"windows,omitempty"`
	ReservationID       string         `json:"reservation_id,omitempty"`
	BudgetReservationID string         `json:"budget_reservation_id,omitempty"`
	BudgetWindows       []BudgetWindow `json:"budget_windows,omitempty"`
	// ResolvedTier is the tier the verdict was evaluated under (#477): explicit
	// req.Tier, else the auto-graduated tier (#476), else the lowest default. The
	// host reads it back to drive its own per-tier capacity decisions (e.g.
	// tensorhub's scheduler in-flight concurrency cap) without a second round-trip.
	ResolvedTier string `json:"resolved_tier,omitempty"`
	// MaxConcurrentHeldAmount is the resolved tier's cap on the sum of the payer's
	// active (un-settled) hold $ (#487; 0 = uncapped); HeldAmount is the active-hold
	// $ sum as evaluated for this verdict (includes the hold just placed on an
	// allow). A host that enforces true occupancy itself reads these to queue
	// against cap + per-job estimate instead of taking the hard deny. An estimate
	// above MaxSingleChargeAmount is denied DenyCode="single_charge_cap_exceeded".
	MaxConcurrentHeldAmount int64 `json:"max_concurrent_held_amount,omitempty"`
	HeldAmount              int64 `json:"held_amount,omitempty"`
	MaxSingleChargeAmount   int64 `json:"max_single_charge_amount,omitempty"`
}

// CaptureUsage carries the analytics dimensions recorded alongside a capture so
// OpenRails can serve per-resource/function/tier/invoker spend (#410). Nil = no
// usage event (a plain capture).
type CaptureUsage struct {
	EventType string
	Resource  string
	Metadata  map[string]any
	Source    string
	SourceID  string
}

// BalanceResponse is the GET /v1/service/credits/balance snapshot (handler
// serviceBalanceResponse). NOTE: the wire field for the owed amount is
// outstanding_owed_amount — go-client's BalanceResponse decoded
// "outstanding_owed_amount"/"remaining_today_amount", which this endpoint never
// sends; fixed here to the handler contract.
type BalanceResponse struct {
	Currency              string `json:"currency"`
	BillingMode           string `json:"billing_mode"`
	BalanceAmount         int64  `json:"balance_amount"`
	HeldAmount            int64  `json:"held_amount"`
	AvailableAmount       int64  `json:"available_amount"`
	OutstandingOwedAmount int64  `json:"outstanding_owed_amount"`
}

// CreditAccount is the OpenRails service balance/policy snapshot.
type CreditAccount struct {
	CustomerID            string `json:"customer_id"`
	Currency              string `json:"currency"`
	BillingMode           string `json:"billing_mode"`
	BalanceAmount         int64  `json:"balance_amount"`
	HeldAmount            int64  `json:"held_amount"`
	AvailableAmount       int64  `json:"available_amount"`
	OutstandingOwedAmount int64  `json:"outstanding_owed_amount"`
}

// AccountSettingsInput patches an OpenRails credit account policy.
type AccountSettingsInput struct {
	BillingMode              *string `json:"billing_mode,omitempty"`
	MaxSpendPerDay           *int64  `json:"max_spend_per_day,omitempty"`
	MaxSpendPerMonth         *int64  `json:"max_spend_per_month,omitempty"`
	MaxOutstandingOwedAmount *int64  `json:"max_outstanding_owed_amount,omitempty"`
	LowBalanceThreshold      *int64  `json:"low_balance_threshold,omitempty"`
	AutoTopupEnabled         *bool   `json:"auto_topup_enabled,omitempty"`
	AutoTopupAmountCents     *int64  `json:"auto_topup_amount_cents,omitempty"`
	AutoTopupPaymentMethod   *string `json:"auto_topup_payment_method_id,omitempty"`
	DefaultCreditExpiryDays  *int    `json:"default_credit_expiry_days,omitempty"`
	HardStopOnBreach         *bool   `json:"hard_stop_on_breach,omitempty"`
	AlertThresholdPct        *int    `json:"alert_threshold_pct,omitempty"`
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
	// Cadence is "session" (default) or "fixed" (#337 fixed per-user-anchored
	// windows): session opens at the user's first charged request and closes
	// WindowSeconds later; fixed ticks at anchor + k*WindowSeconds forever.
	Cadence string `json:"cadence,omitempty"`
}

// BudgetWindow is one computed window from POST /v1/service/budget/check,
// GET /v1/service/budget, and Admit's budget_windows
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
	Cadence string    `json:"cadence,omitempty"`
	Allowed bool      `json:"allowed"`
}

// TierThroughputWindow is one tier throughput limit: at most Max units of Unit
// per WindowSeconds (pkg/service.TierWindowInput on the wire).
type TierThroughputWindow struct {
	Unit          string `json:"unit"`
	WindowSeconds int64  `json:"window_seconds"`
	Max           int64  `json:"max"`
}

// TierQueueLimit is one batch/queue reservation-pool cap (#472 G2 / #477): at
// most Max units of Unit in-flight per (payer, endpoint-release). No time
// window — held while the job is pending, freed at settlement
// (pkg/service.TierQueueLimitInput on the wire).
type TierQueueLimit struct {
	Unit string `json:"unit"`
	Max  int64  `json:"max"`
}

// TierPolicyInput configures a per-payer tier policy via
// PUT /v1/service/tier-policies (#298): throughput windows + entitled
// resources + fixed money-budget windows + flex/batch queue limits
// (pkg/service.TierPolicyInput on the wire; customer_id travels alongside
// it).
type TierPolicyInput struct {
	Tier              string                 `json:"tier"`
	Windows           []TierThroughputWindow `json:"windows"`
	EntitledResources []string               `json:"entitled_resources"`
	BudgetWindows     []BudgetWindowInput    `json:"budget_windows"`
	// QueueLimits are the batch/flex reservation-pool caps (#472 G2 / #477): the
	// per-tier in-flight queue ceiling OpenRails enforces at admit (BlockedBy=
	// "queue" on overflow). Empty = no queue cap for this tier.
	QueueLimits []TierQueueLimit `json:"queue_limits,omitempty"`
	// MaxConcurrentHeldAmount / MaxSingleChargeAmount are the #487 generic
	// $-denominated per-tier admit limits (0 = uncapped): a cap on the payer's
	// active (un-settled) hold $ sum (surfaced + enforced at admit), and a
	// per-charge ceiling (an over-limit estimate is rejected at admit).
	MaxConcurrentHeldAmount int64 `json:"max_concurrent_held_amount,omitempty"`
	MaxSingleChargeAmount   int64 `json:"max_single_charge_amount,omitempty"`
	// BadSpendWindows are the #497 per-PAYER direct-credential wasted-spend grace
	// windows for this tier: at most Limit of host-reported wasted spend is
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
type WastedSpendReport struct {
	CustomerID  string `json:"customer_id"`
	Invoker     string `json:"invoker"`
	InvokerType string `json:"invoker_type,omitempty"`
	Currency    string `json:"currency,omitempty"`
	Amount      int64  `json:"amount"`
	Source      string `json:"source"`
	SourceID    string `json:"source_id"`
	Reason      string `json:"reason,omitempty"`
}

// WastedSpendResponse reports how OpenRails handled a wasted-spend report.
type WastedSpendResponse struct {
	Currency       string `json:"currency"`
	RecordedAmount int64  `json:"recorded_amount"`
	ForgivenAmount int64  `json:"forgiven_amount"`
	ChargedAmount  int64  `json:"charged_amount"`
	Action         string `json:"action"`
	Duplicate      bool   `json:"duplicate,omitempty"`
}

// TierScheduleRung is one rung of the persisted tier ladder set via
// PUT /v1/service/tier-schedules (#476): a payer reaches Tier once its
// cumulative paid spend is at least MinCumulativePaidAmount. Order ascending by
// MinCumulativePaidAmount (the server sorts defensively regardless).
type TierScheduleRung struct {
	Tier                    string `json:"tier"`
	MinCumulativePaidAmount int64  `json:"min_cumulative_paid_amount"`
}

// BudgetScopeWindow is one fixed money-budget window in a hierarchical
// budget-scope policy (#473) — same shape as BudgetWindowInput
// (pkg/service.BudgetScopeWindowInput on the wire).
type BudgetScopeWindow struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	// Cadence is "session" (default) or "fixed" (#337 fixed windows).
	Cadence string `json:"cadence,omitempty"`
}

// SubjectBudgetPolicyInput configures a SUBJECT-owned hierarchical budget-scope
// policy via PUT /v1/service/budget-policies/subject (#473). Scope is
// "subject" (the subject's self cap) or "role" (a (subject, role) pool); RoleID
// is the role uuid when scope=role, empty otherwise. The owner is fixed to
// "subject" server-side (pkg/service.BudgetScopePolicyInput on the wire;
// customer_id travels alongside it).
type SubjectBudgetPolicyInput struct {
	Scope   string              `json:"scope"`
	RoleID  string              `json:"role_id,omitempty"`
	Windows []BudgetScopeWindow `json:"windows"`
}

// PlatformBudgetPolicyInput configures a PLATFORM-owned budget cap via
// PUT /v1/service/budget-policies/platform (#473): the platform->payer cap
// (scope=subject). The owner is fixed to "platform" server-side.
type PlatformBudgetPolicyInput struct {
	Scope   string              `json:"scope"`
	Windows []BudgetScopeWindow `json:"windows"`
}

// SubjectBudgetPolicy is one subject-owned budget-scope policy returned by
// SubjectBudgetPolicies (#473) — the read-back shape for host reconciliation.
type SubjectBudgetPolicy struct {
	Scope   string              `json:"scope"`
	RoleID  string              `json:"role_id,omitempty"`
	Windows []BudgetScopeWindow `json:"windows"`
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

// OpenWindowRequest is the body for POST /v1/service/credits/windows (#335).
type OpenWindowRequest struct {
	CustomerID string `json:"customer_id"`
	Invoker    string `json:"invoker"`
	// Currency is the ledger unit; empty defaults to "USD" (#476).
	Currency   string `json:"currency,omitempty"`
	Amount     int64  `json:"amount"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"`
}

// CreditWindow is the window snapshot returned by open/refill/close
// (pkg/service.CreditWindowDTO on the wire).
type CreditWindow struct {
	WindowID      uuid.UUID `json:"window_id"`
	CustomerID    uuid.UUID `json:"customer_id"`
	Currency      string    `json:"currency"`
	HeldAmount    int64     `json:"held_amount"`
	SettledAmount int64     `json:"settled_amount"`
	Status        string    `json:"status"` // open | closed | expired
	ExpiresAt     time.Time `json:"expires_at"`
}

// WindowSettleUsage carries the analytics attribution recorded with a settled
// item (no second debit) — the window analogue of CaptureUsage.
type WindowSettleUsage struct {
	EventType string         `json:"event_type"`
	Resource  string         `json:"resource,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// WindowSettleItem is one settled actual. Items in one SettleWindowItems call
// may span windows AND payers (the cross-payer flush). RequestID is the
// per-item idempotency key.
type WindowSettleItem struct {
	WindowID  uuid.UUID          `json:"window_id"`
	RequestID string             `json:"request_id"`
	Amount    int64              `json:"amount"`
	Invoker   string             `json:"invoker,omitempty"`
	Usage     *WindowSettleUsage `json:"usage,omitempty"`
}

// WindowSettleResult is one per-item settle outcome. OK with Replayed=true is
// an idempotent replay (already settled, not charged again). Error is one of
// window_not_found | window_not_open | window_exceeded | invalid_item |
// internal_error.
type WindowSettleResult struct {
	WindowID      uuid.UUID  `json:"window_id"`
	RequestID     string     `json:"request_id"`
	OK            bool       `json:"ok"`
	Replayed      bool       `json:"replayed,omitempty"`
	Error         string     `json:"error,omitempty"`
	TransactionID *uuid.UUID `json:"transaction_id,omitempty"`
}

// AdmitBatchVerdict is one per-item verdict from POST /v1/service/admit/batch.
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
