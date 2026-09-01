// Package openrails is the canonical OpenRails SDK surface (#338): ONE Go
// interface (Client) with two constructors —
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
// This root package stays dependency-light: it MUST NOT import internal/* or
// pkg/embedded, so a remote-only consumer's binary does not link the engine
// (enforced by deps_test.go).
package openrails

import (
	"context"
	"fmt"
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

// WithMerchant pins a per-call merchant onto ctx for merchant-scoped SDK
// calls. Semantics differ by transport:
//
//   - REMOTE client (NewRemote): a no-op. Merchant identity comes from the
//     service credential (the Bearer token), never the ctx.
//   - EMBEDDED client (openrails/embed): the engine binds to ONE merchant no
//     later than its first UpsertMerchantConfig. Before that bind, a
//     WithMerchant pin is honored per call. Once bound, a ctx pin that agrees
//     with the bound merchant is a no-op; a ctx pin naming a DIFFERENT
//     merchant errors, naming both merchants (#772) — one embedded engine
//     serves one merchant, so a mismatched pin is refused rather than
//     silently executed against the bound merchant.
func WithMerchant(ctx context.Context, id MerchantID) context.Context {
	return merchant.WithID(ctx, id)
}

// AdmissionClient is the Tensorhub hot path: batch admission, settle, release,
// wasted-spend reporting, and trust-level read.
type AdmissionClient interface {
	AdmitBatch(ctx context.Context, items []AdmitRequest) ([]AdmitBatchVerdict, error)
	// Capture settles the admission/authorize hold request_id at the actual
	// amount, optionally recording a usage analytics event (#311/#410).
	Capture(ctx context.Context, requestID string, capturedAmount int64, usage *CaptureUsage) error
	// Release frees the admission/authorize hold request_id without charging.
	Release(ctx context.Context, requestID string) error
	// ExtendHold re-declares the deadline of the live hold request_id: the job
	// it covers is still running and will now finish by expiresAt. A hold
	// lives exactly as long as its owner declared (AdmitRequest.ExpiresAt,
	// required with EstimatedAmount) — there is no default — so a job that
	// outlives its estimate extends before the deadline or loses the hold.
	// ErrNotFound when nothing live exists to extend (captured, released, or
	// lapsed): re-admit; a lapsed hold is never resurrected.
	ExtendHold(ctx context.Context, requestID string, expiresAt time.Time) error
	// GetTrustLevel returns the payer's current trust level (#477) for one currency:
	// the value OpenRails auto-maintains from same-currency cumulative paid spend
	// against the persisted schedule (#476), or a manual admin override. Empty
	// means the host treats it as the lowest/default trust level.
	GetTrustLevel(ctx context.Context, customerID, currency string) (string, error)
	// ReportWastedSpend records host-reported WASTED $ (#497): delegated invokers
	// accrue toward their flat cutoff; direct payer credentials use trust-level
	// grace and charge overage through the normal ledger. Source+SourceID are
	// required for retry idempotency.
	ReportWastedSpend(ctx context.Context, report WastedSpendReport) (*WastedSpendResponse, error)
}

// UsageReportClient reports metered usage events outside the admission
// hold/capture cycle (#797): the host records a usage_events row (optionally
// host-priced via Amount; 0 = free/metered-only) that the rate-card rating
// sweep aggregates into arrears invoice lines. Gauge meters (GB-month style)
// report unit-second segment quantities in Dimensions.
type UsageReportClient interface {
	RecordUsage(ctx context.Context, report UsageReport) error
}

// PolicySyncClient installs merchant-owned admission policy in one settings
// document.
type PolicySyncClient interface {
	GetMerchantSettings(ctx context.Context) (*MerchantSettings, error)
	SetMerchantSettings(ctx context.Context, settings MerchantSettings) error
	// SetCustomerSpendDelegations explicitly replaces the customer's complete
	// delegation document.
	SetCustomerSpendDelegations(ctx context.Context, customerID string, delegations []SpendDelegationInput) error
	// SetCustomerSpendDelegation atomically upserts one delegation without
	// reading or replacing unrelated customer delegations.
	SetCustomerSpendDelegation(ctx context.Context, customerID string, delegation SpendDelegationInput) error
	// DeleteCustomerSpendDelegation revokes exactly ONE addressed delegation
	// (or#911) and leaves every sibling untouched. A missing grant (already
	// revoked or never granted) returns an error matching ErrNotFound.
	DeleteCustomerSpendDelegation(ctx context.Context, customerID, scope, scopeKey string) error
}

// AdminFundingClient is the small non-hot-path funding/reporting surface used
// by standalone admin jobs.
type AdminFundingClient interface {
	// DepositCredits mints a credit block for a payer (admin funding, promotions,
	// money-in settlement). Returns the ledger transaction created.
	DepositCredits(ctx context.Context, req DepositCreditsRequest) (*CreditTransaction, error)
	// GetDeposit answers "what did this deposit key do" (or#906): the grant
	// committed at (customerID, sourceID) — id, amount, created_at, with
	// Replayed=true — or an error matching ErrNotFound when the key never
	// committed. Key-qualified: sourceID is the caller half of the deposit
	// idempotency key; the operation half is deposit by construction.
	GetDeposit(ctx context.Context, customerID, sourceID string) (*CreditTransaction, error)
	// SetCreditLimit sets the admin-managed arrears credit line for a payer in one
	// currency. A zero limit removes the credit line.
	SetCreditLimit(ctx context.Context, customerID, currency string, creditLimit int64) error
	// GetCreditLimit reads the admin-managed arrears credit line for a payer in one
	// currency (0 = no credit line). Read counterpart of SetCreditLimit (#489); the
	// limit is not surfaced by GetCreditAccount/settings, hence its own call.
	GetCreditLimit(ctx context.Context, customerID, currency string) (int64, error)
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
	UsageReportClient
	PolicySyncClient
	AdminFundingClient
	CustomerLookupClient
}

// Verifier is implemented by clients that support an authenticated readiness
// probe (both constructors' clients do). Kept OUT of Client so existing
// third-party implementations of the interface keep compiling.
type Verifier interface {
	// Verify checks reachability AND credential validity via one cheap
	// authenticated call (the db.Ping pattern). Call it from main for
	// fail-fast-at-boot; constructors stay I/O-free.
	Verify(ctx context.Context) error
}

// Verify runs the client's authenticated readiness probe (see Verifier).
func Verify(ctx context.Context, c Client) error {
	v, ok := c.(Verifier)
	if !ok {
		return fmt.Errorf("openrails: client does not support Verify")
	}
	return v.Verify(ctx)
}

// SelfIssuer is the issuer keying customers rows for self-service
// identities whose subject is the user's own UUID — what an embedded host
// passes to ListActiveEntitlements for its own users (internal/db
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
	// Amount differs is refused with ErrIdempotencyKeyReused (HTTP 409).
	SourceID    string
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
	// Replayed reports that this write's idempotency key had ALREADY committed,
	// so nothing moved in THIS call — the row described here is the movement
	// that landed earlier (or#892). Serialized by the engine on both transports;
	// a consumer that needs applied-vs-replayed reads it here instead of keeping
	// its own claim table.
	Replayed bool
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
	EstimatedAmount int64  `json:"estimated_amount"`
	// AccrualRateDeltaPerHour is the or#897 PROSPECTIVE rate this request would
	// add, in micros per hour — "the VM I am about to start burns $2/hour". Only
	// the host knows it. Zero means the request adds no ongoing rate, which
	// leaves an accrual_rate_cap payer gated on what is already running.
	AccrualRateDeltaPerHour int64  `json:"accrual_rate_delta_per_hour,omitempty"`
	RequestID               string `json:"request_id"`
	Source                  string `json:"source,omitempty"`
	// ExpiresAt (unix seconds) is the deadline of the job this admit covers.
	// REQUIRED when EstimatedAmount places a hold: the hold lives that long
	// unless captured, released or extended (ExtendHold). Refused otherwise.
	ExpiresAt *int64 `json:"expires_at,omitempty"`
	// Roles are the immutable role UUIDs the invoker holds (#473). Each role with a
	// matching (subject, role) budget-scope policy gates this request's spend in
	// the same admit verdict. The host reads them from the delegated
	// JWT/permission set. Empty = no role-scoped budget applies.
	Roles []uuid.UUID `json:"roles,omitempty"`
}

// AdmitResponse is the admission verdict (pkg/service.AdmitResult on the wire).
// Allowed=false carries a BlockedBy axis ("budget" | "abuse" | "money") and a
// DenyCode when available. A successful money-bearing admit creates a request_id
// keyed Redis hold. A deny is returned as (Allowed=false, nil error) on both
// transports even though HTTP maps it to 402/403/429.
type AdmitResponse struct {
	Allowed             bool       `json:"allowed"`
	BlockedBy           string     `json:"blocked_by,omitempty"`
	DenyCode            string     `json:"deny_code,omitempty"`
	Currency            string     `json:"currency,omitempty"`
	EstimatedAmount     int64      `json:"estimated_amount,omitempty"`
	StartCapacityAmount int64      `json:"start_capacity_amount,omitempty"`
	RetryAfterSeconds   int64      `json:"retry_after_seconds,omitempty"`
	HoldExpiresAt       *time.Time `json:"hold_expires_at,omitempty"`
}

// CaptureUsage carries the analytics dimensions recorded alongside a capture so
// OpenRails can serve per-resource/function/tier/invoker spend (#410). Nil = no
// usage event (a plain capture).
//
// It also carries OPTIONAL fallback payer coordinates (#676): the admit-time
// request→payer pointer lives in Redis and can be lost (flush/failover/TTL
// overrun). Supplying CustomerID+Currency lets the capture land anyway — a
// rendered service is always chargeable. Retries dedupe on the request id
// alone (or#907); nothing here participates in the idempotency coordinate.
// These fields are independent of EventType.
type CaptureUsage struct {
	// CustomerID/Currency/Invoker are the #676 capture-durability fallback
	// coordinates (see type doc). Optional; ignored while the admit pointer is
	// live.
	CustomerID string
	Currency   string
	Invoker    string

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
	SignupURL   string `json:"signup_url,omitempty"`
}

// MerchantSettings is the merchant-owned admission/policy document installed by
// standalone policy sync jobs.
type MerchantSettings struct {
	Profile             *MerchantProfileInput        `json:"profile,omitempty"`
	TrustLevelSchedules []MerchantTrustLevelSchedule `json:"trust_level_schedules,omitempty"`
	// BillingPolicies / BillingPolicyBindings are the or#897 registry: named
	// policies and the rungs that decide who gets which. They REPLACE the retired
	// trust_level_spend_limits field, which could only ever mean "window cap".
	BillingPolicies                   []BillingPolicyInput        `json:"billing_policies,omitempty"`
	BillingPolicyBindings             []BillingPolicyBindingInput `json:"billing_policy_bindings,omitempty"`
	DelegatedInvokerWastedSpendLimits []BudgetWindowInput         `json:"delegated_invoker_wasted_spend_limits,omitempty"`
}

// MerchantTrustLevelSchedule is one currency's trust-level ladder.
type MerchantTrustLevelSchedule struct {
	Currency string                   `json:"currency"`
	Schedule []TrustLevelScheduleRung `json:"schedule"`
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
	OutstandingCapAmount int64 `json:"outstanding_cap_amount,omitempty"`
	// SpendWindows are the rolling NEW-spend ceilings for kind=window_spend_cap.
	SpendWindows []BudgetWindowInput `json:"spend_windows,omitempty"`
	// AccrualRateCapPerHour (kind=accrual_rate_cap) caps the measured accrual
	// rate in micros PER HOUR — the cloud quota. AccrualRateWindowSeconds is the
	// measurement lookback (default 3600).
	AccrualRateCapPerHour    int64 `json:"accrual_rate_cap_per_hour,omitempty"`
	AccrualRateWindowSeconds int64 `json:"accrual_rate_window_seconds,omitempty"`
	// CollectionThresholdAmount / DelinquencyGraceDays / DelinquencyAmountFloor
	// override the merchant-wide invoice policy for payers bound here; nil defers
	// to it. All three ride on any kind.
	CollectionThresholdAmount *int64 `json:"collection_threshold_amount,omitempty"`
	DelinquencyGraceDays      *int   `json:"delinquency_grace_days,omitempty"`
	DelinquencyAmountFloor    *int64 `json:"delinquency_amount_floor,omitempty"`
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

// BillingPolicyBindingInput points one rung at a declared policy name (or#897).
// Set CustomerID for the per-customer rung, Tier for the per-tier rung, neither
// for the merchant default — never both. Most specific wins.
//
// GetMerchantSettings returns only the DECLARATIVE rungs (default + tier):
// per-customer bindings are runtime segmentation state and are not enumerated.
type BillingPolicyBindingInput struct {
	PolicyName string `json:"policy"`
	CustomerID string `json:"customer_id,omitempty"`
	Tier       string `json:"tier,omitempty"`
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
	Amount int64 `json:"amount"`
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
// through rate-card rating). OccurredAtUnix (seconds; 0 = now) places the
// event in its rating window — gauge segment reporters set it to segment end.
type UsageReport struct {
	CustomerID string           `json:"customer_id"`
	Invoker    string           `json:"invoker"`
	Currency   string           `json:"currency,omitempty"`
	EventType  string           `json:"event_type"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
	Amount     int64            `json:"amount"`
	Resource   string           `json:"resource,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
	Source     string           `json:"source"`
	SourceID   string           `json:"source_id"`
	// OccurredAtUnix is the event time as a unix timestamp in seconds (0 = now).
	OccurredAtUnix int64 `json:"occurred_at_unix,omitempty"`
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

// TrustLevelScheduleRung is one rung of the persisted same-currency trust-level
// ladder set via merchant settings (#476): a payer reaches TrustLevel once its
// cumulative paid spend in the schedule currency is at least
// MinCumulativePaidAmount. Order ascending by MinCumulativePaidAmount (the
// server sorts defensively regardless).
type TrustLevelScheduleRung struct {
	TrustLevel              string `json:"trust_level"`
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
