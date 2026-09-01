package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AdmitInput is the host's admission request for payer capacity and delegated
// spend gates.
type AdmitInput struct {
	CustomerID  identity.CustomerID
	Invoker     string
	InvokerType string
	TrustLevel  string // payer trust level
	Resource    string
	// Roles are the immutable role UUIDs the invoker holds (#473). Each role with a
	// matching (subject, role) budget policy gates this request's spend. The host
	// reads them from the delegated JWT/permission set.
	Roles           []uuid.UUID
	Currency        string
	EstimatedAmount int64
	// AccrualRateDeltaPerHour is the or#897 PROSPECTIVE rate this request would
	// add, in micros per hour — "the VM I am about to start burns $2/hour". Only
	// the host knows it. Zero means the request adds no ongoing rate, which
	// leaves an accrual_rate_cap payer gated on what is already running.
	AccrualRateDeltaPerHour int64
	Source                  string
	SourceID                string
	// ExpiresAtUnix is the deadline of the job this admit covers — REQUIRED
	// when EstimatedAmount places a hold; the hold lives exactly that long
	// unless captured, released or extended.
	ExpiresAtUnix int64
}

// ErrHoldDeadlineRequired is returned by Admit when EstimatedAmount places a
// hold and no ExpiresAtUnix was declared.
var ErrHoldDeadlineRequired = admission.ErrHoldDeadlineRequired

// ErrHoldDeadlinePassed is returned by Admit/ExtendHold when the declared
// deadline is already in the past.
var ErrHoldDeadlinePassed = admission.ErrHoldDeadlinePassed

// ErrHoldNotFound is returned by ExtendHold when no live hold exists for the
// request id: it was captured, released, or lapsed at its declared deadline.
// The caller must re-admit; a lapsed hold is never resurrected.
var ErrHoldNotFound = errors.New("hold not found for request_id")

// AdmitResult is the unified admission decision returned to the host.
type AdmitResult struct {
	Allowed             bool       `json:"allowed"`
	Currency            string     `json:"currency,omitempty"`
	EstimatedAmount     int64      `json:"estimated_amount,omitempty"`
	StartCapacityAmount int64      `json:"start_capacity_amount,omitempty"`
	BlockedBy           string     `json:"blocked_by,omitempty"`
	DenyCode            string     `json:"deny_code,omitempty"`
	RetryAfterSeconds   int64      `json:"retry_after_seconds,omitempty"`
	HoldExpiresAt       *time.Time `json:"hold_expires_at,omitempty"`
}

// Admit runs service admission: the delegated wasted-spend cutoff + the single
// atomic spendgate EVAL (affordability + spend-cap windows + Redis hold). The gate
// + Postgres→policy loader are built from the runtime per call (both cheap,
// stateless). The payer's Postgres money lock spans the final capacity read and
// Redis reserve so durable and Redis reservations cannot consume the same funds;
// there are still no per-request Postgres reservation rows.
func (s *Service) Admit(ctx context.Context, in AdmitInput) (*AdmitResult, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if s.rt.RedisClient == nil {
		return nil, fmt.Errorf("admission unavailable: redis not configured")
	}
	if in.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	if in.EstimatedAmount > 0 {
		in.SourceID = strings.TrimSpace(in.SourceID)
		if in.SourceID == "" {
			return nil, fmt.Errorf("request_id required")
		}
		in.InvokerType = strings.TrimSpace(in.InvokerType)
		if in.InvokerType != string(identity.InvokerTypePayer) && in.InvokerType != string(identity.InvokerTypeDelegated) {
			return nil, fmt.Errorf("invoker_type must be payer or delegated")
		}
	}
	currency, err := requireCurrency(in.Currency)
	if err != nil {
		return nil, err
	}

	gate := spendgate.New(s.rt.RedisClient)
	loader := admission.NewSpendgatePolicyLoader(
		admission.NewBillingPolicyStore(s.rt.DB),
		admission.NewInvokerSpendLimitStore(s.rt.DB),
		s.rt.FXProvider,
	).WithCache(s.rt.AdmissionPolicyCache)
	invokerWindows, err := s.invokerWastedSpendPolicy(ctx)
	if err != nil {
		return nil, err
	}
	adm := admission.NewAdmitter(s.moneyService(), gate, loader).
		WithWastedSpend(abuse.NewWastedSpendGuard(ratelimit.NewLimiter(s.rt.RedisClient)), invokerWindows).
		WithDenialRecorder(admission.NewDenialRecorder(s.rt.RedisClient)).
		WithDelinquency(s.delinquencyService()).
		WithAccrualRateMeter(admission.NewAccrualRateMeter(s.rt.DB))

	// The hold's lifetime is the caller's declared job deadline, never a
	// default of ours (xs-007 row 33): the Admitter refuses a hold that
	// declares none (ErrHoldDeadlineRequired). A job that outlives its
	// estimate re-declares through ExtendHold.
	var exp time.Time
	if in.ExpiresAtUnix > 0 {
		exp = time.Unix(in.ExpiresAtUnix, 0).UTC()
	}
	source := in.Source
	if source == "" {
		source = "admit"
	}

	dec, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID:      in.CustomerID,
		Invoker:         in.Invoker,
		InvokerType:     in.InvokerType,
		TrustLevel:      in.TrustLevel,
		Resource:        in.Resource,
		Roles:           in.Roles,
		Currency:        currency,
		EstimatedAmount: in.EstimatedAmount,

		AccrualRateDeltaPerHour: in.AccrualRateDeltaPerHour,
		Source:                  source,
		SourceID:                in.SourceID,
		ExpiresAt:               exp,
	})
	if err != nil {
		return nil, err
	}

	res := &AdmitResult{
		Allowed:             dec.Allowed,
		Currency:            currency,
		EstimatedAmount:     in.EstimatedAmount,
		StartCapacityAmount: startCapacity(dec.AvailableAmount, dec.HeldAmount),
		BlockedBy:           dec.BlockedBy,
		DenyCode:            dec.DenyCode,
		RetryAfterSeconds:   dec.RetryAfterSeconds,
	}
	if dec.Allowed && in.EstimatedAmount > 0 {
		res.HoldExpiresAt = &exp
	}
	return res, nil
}

func startCapacity(accountCapacity, activeHeld int64) int64 {
	if activeHeld < 0 {
		activeHeld = 0
	}
	if activeHeld >= accountCapacity {
		return 0
	}
	return accountCapacity - activeHeld
}

// SpendLimitWindowInput is one fixed money-budget window: at most Limit of
// spend per WindowSeconds. The single window shape in this package — used by
// budget-scope policies (#473) and by billing-policy spend/bad-spend windows
// (or#897).
type SpendLimitWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}

// InvokerSpendLimitInput configures one hierarchical budget-scope policy (#473).
// Scope is "subject" | "role" | "invoker" | "invoker_tier"; ScopeKey is the
// role uuid, invoker string, or invoker-tier key, empty for scope=subject.
// Provenance (or#911) is the caller's opaque reference for what authorized the
// grant (e.g. a signed-document digest); stored on the grant, returned on
// reads, never interpreted.
type InvokerSpendLimitInput struct {
	Scope      string                  `json:"scope"`
	ScopeKey   string                  `json:"scope_key,omitempty"`
	Windows    []SpendLimitWindowInput `json:"windows"`
	Provenance string                  `json:"provenance,omitempty"`
}

// ErrInvalidInvokerSpendLimit identifies caller-owned spend-delegation input
// errors so HTTP and embedded transports can map the shared service result to
// the same 400/ErrInvalid contract.
var ErrInvalidInvokerSpendLimit = errors.New("invalid invoker spend limit")

type invokerSpendLimitValidationError struct{ message string }

func (e *invokerSpendLimitValidationError) Error() string { return e.message }
func (e *invokerSpendLimitValidationError) Unwrap() error { return ErrInvalidInvokerSpendLimit }

func invalidInvokerSpendLimit(message string) error {
	return &invokerSpendLimitValidationError{message: message}
}

func budgetScopeWindowModels(ws []SpendLimitWindowInput) []models.BudgetWindowPolicy {
	out := make([]models.BudgetWindowPolicy, 0, len(ws))
	for _, w := range ws {
		out = append(out, models.BudgetWindowPolicy{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency})
	}
	return out
}

func spendLimitWindowInputs(ws []models.BudgetWindowPolicy) []SpendLimitWindowInput {
	out := make([]SpendLimitWindowInput, 0, len(ws))
	for _, w := range ws {
		out = append(out, SpendLimitWindowInput{
			Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency,
		})
	}
	return out
}

func invokerSpendLimitKey(scope, scopeKey string) string {
	return budgets.NormalizeScope(scope) + "\x00" + strings.TrimSpace(scopeKey)
}

// ValidateInvokerSpendLimitInputs validates and canonicalizes a complete
// payer-owned spend-delegation document. Duplicate detection happens after
// scope and scope_key are normalized, so every transport has identical
// replacement semantics. or#893 deleted the role_id alias: a role delegation is
// {scope:"role", scope_key:"<role uuid>"} and nothing else.
func ValidateInvokerSpendLimitInputs(in []InvokerSpendLimitInput) ([]InvokerSpendLimitInput, error) {
	out := make([]InvokerSpendLimitInput, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, item := range in {
		scope := budgets.NormalizeScope(item.Scope)
		scopeKey := strings.TrimSpace(item.ScopeKey)
		row, err := admission.ValidateInvokerSpendLimit(admission.InvokerSpendLimit{
			Scope: scope, ScopeKey: scopeKey, Windows: budgetScopeWindowModels(item.Windows),
			Provenance: item.Provenance,
		})
		if err != nil {
			return nil, invalidInvokerSpendLimit(fmt.Sprintf("delegations[%d].%s", i, err))
		}
		key := invokerSpendLimitKey(row.Scope, row.ScopeKey)
		if _, duplicate := seen[key]; duplicate {
			return nil, invalidInvokerSpendLimit(fmt.Sprintf("duplicate delegation for %s", key))
		}
		seen[key] = struct{}{}
		out = append(out, InvokerSpendLimitInput{
			Scope: row.Scope, ScopeKey: row.ScopeKey, Windows: spendLimitWindowInputs(row.Windows),
			Provenance: row.Provenance,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return invokerSpendLimitKey(out[i].Scope, out[i].ScopeKey) < invokerSpendLimitKey(out[j].Scope, out[j].ScopeKey)
	})
	return out, nil
}

func invokerSpendLimitRow(in InvokerSpendLimitInput) admission.InvokerSpendLimit {
	return admission.InvokerSpendLimit{
		Scope: in.Scope, ScopeKey: in.ScopeKey, Windows: budgetScopeWindowModels(in.Windows),
		Provenance: in.Provenance,
	}
}

// SetInvokerSpendLimits upserts a SUBJECT-owned budget-scope policy (#473): the
// subject's self cap, a role pool, an invoker grant, or an invoker-tier grant.
// Payer-set: the payer caps how much its delegated invokers/roles may spend.
func (s *Service) SetInvokerSpendLimits(ctx context.Context, payer identity.CustomerID, in InvokerSpendLimitInput) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	next, err := ValidateInvokerSpendLimitInputs([]InvokerSpendLimitInput{in})
	if err != nil {
		return err
	}
	return admission.NewInvokerSpendLimitStore(s.rt.DB).Upsert(ctx, payer, invokerSpendLimitRow(next[0]))
}

// InvokerSpendWindowsInput names the invoker whose live spend windows to read.
// The caller supplies the identity from its AUTH seam, never from the wire —
// this read answers "what am I metered against", so an invoker the caller could
// name would be somebody else's budget.
type InvokerSpendWindowsInput struct {
	Invoker string
	// Roles are the invoker's immutable role UUIDs, so role-scoped grants it
	// holds are included exactly as the admit path includes them (#473).
	Roles []uuid.UUID
	// Currency the limits are reported in; the spendgate meters one currency per
	// payer, and a window declared in another is FX-converted the same way admit
	// converts it. Defaults to the service currency.
	Currency string
	// TrustLevel selects invoker_tier grants; empty resolves the payer's live
	// level exactly as admission does.
	TrustLevel string
}

// InvokerSpendWindow is one delegated spend window with its live metering.
// Used is the window's current total; because windows are estimate-based it
// already includes in-flight reservations, and Reserved names that part.
// Remaining is what the gate would still admit.
type InvokerSpendWindow struct {
	Scope         string    `json:"scope"`
	Key           string    `json:"key"`
	WindowSeconds int64     `json:"window_seconds"`
	Limit         int64     `json:"limit"`
	Currency      string    `json:"currency"`
	Used          int64     `json:"used"`
	Reserved      int64     `json:"reserved"`
	Remaining     int64     `json:"remaining"`
	ResetsAt      time.Time `json:"resets_at"`
}

// InvokerSpendWindows returns the spend windows a delegated invoker is enforced
// against on payer's account, with their live metering (or#930).
//
// It is a READ over the accounting admission already keeps: the same grants the
// admit path resolves (LoadDelegatedWindows), metered by the same Redis counters
// and hold records the gate writes. Nothing here counts anything.
//
// PAYER-SCOPE WINDOWS ARE DELIBERATELY ABSENT. The payer's own caps and product
// usage limits gate the whole account, not this invoker; showing them to one
// delegated user would report a budget it neither owns nor can act on, and would
// leak the account's aggregate posture. The payer reads those as the payer.
func (s *Service) InvokerSpendWindows(ctx context.Context, payer identity.CustomerID, in InvokerSpendWindowsInput) ([]InvokerSpendWindow, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if s.rt.RedisClient == nil {
		return nil, fmt.Errorf("spend windows unavailable: redis not configured")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	invoker := strings.TrimSpace(in.Invoker)
	if invoker == "" {
		return nil, fmt.Errorf("invoker required")
	}
	currency, err := requireCurrency(in.Currency)
	if err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}

	trustLevel := strings.TrimSpace(in.TrustLevel)
	if trustLevel == "" {
		if t, terr := s.moneyService().GetTrustLevel(ctx, payer, currency); terr == nil && t != "" {
			trustLevel = t
		}
		if trustLevel == "" {
			trustLevel = admission.DefaultTrustLevel
		}
	}

	roles := make([]string, 0, len(in.Roles))
	for _, role := range in.Roles {
		roles = append(roles, role.String())
	}
	req := spendgate.Request{Invoker: invoker, TrustLevel: trustLevel, Roles: roles}
	loader := admission.NewSpendgatePolicyLoader(
		admission.NewBillingPolicyStore(s.rt.DB),
		admission.NewInvokerSpendLimitStore(s.rt.DB),
		s.rt.FXProvider,
	).WithCache(s.rt.AdmissionPolicyCache)
	scopes, _, err := loader.LoadDelegatedWindows(ctx, payer, trustLevel, currency, req)
	if err != nil {
		return nil, err
	}

	usage, err := spendgate.New(s.rt.RedisClient).WindowUsage(
		ctx, tid.UUID().String(), payer.UUID().String(), currency, spendgate.Policy{Scopes: scopes}, req)
	if err != nil {
		return nil, err
	}

	out := make([]InvokerSpendWindow, 0, len(usage))
	for _, u := range usage {
		remaining := u.Limit - u.Used
		if remaining < 0 {
			remaining = 0
		}
		out = append(out, InvokerSpendWindow{
			Scope:         string(u.Scope),
			Key:           u.Key,
			WindowSeconds: int64(u.Duration / time.Second),
			Limit:         u.Limit,
			Currency:      currency,
			Used:          u.Used,
			Reserved:      u.Reserved,
			Remaining:     remaining,
			ResetsAt:      u.ResetsAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// InvokerSpendLimits returns the payer's per-invoker spend limits (#473/#517).
func (s *Service) InvokerSpendLimits(ctx context.Context, payer identity.CustomerID) ([]InvokerSpendLimitInput, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	rows, err := admission.NewInvokerSpendLimitStore(s.rt.DB).LoadAll(ctx, payer)
	if err != nil {
		return nil, err
	}
	out := make([]InvokerSpendLimitInput, 0, len(rows))
	for _, r := range rows {
		w := make([]SpendLimitWindowInput, 0, len(r.Windows))
		for _, ww := range r.Windows {
			w = append(w, SpendLimitWindowInput{Key: ww.Key, WindowSeconds: ww.WindowSeconds, Limit: ww.Limit, Currency: ww.Currency})
		}
		out = append(out, InvokerSpendLimitInput{Scope: r.Scope, ScopeKey: r.ScopeKey, Windows: w, Provenance: r.Provenance})
	}
	return out, nil
}

// DeleteInvokerSpendLimit revokes exactly ONE addressed delegation (or#911) and
// leaves every sibling untouched — the single-grant delete a replace-all cannot
// express without clobbering unrelated grants, and the zero-limit-window
// workaround existed to approximate. Returns whether a grant existed at
// (scope, scope_key); false is a real answer (already revoked or never
// granted), not an error.
func (s *Service) DeleteInvokerSpendLimit(ctx context.Context, payer identity.CustomerID, scope, scopeKey string) (bool, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return false, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return false, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return false, fmt.Errorf("payer required")
	}
	scope = budgets.NormalizeScope(scope)
	switch scope {
	case budgets.ScopeInvoker, budgets.ScopeRole, budgets.ScopeInvokerTrustLevel:
	default:
		return false, invalidInvokerSpendLimit(fmt.Sprintf("scope must be %q, %q, or %q", budgets.ScopeInvoker, budgets.ScopeRole, budgets.ScopeInvokerTrustLevel))
	}
	scopeKey = strings.TrimSpace(scopeKey)
	if scopeKey == "" {
		return false, invalidInvokerSpendLimit("scope_key required")
	}
	return admission.NewInvokerSpendLimitStore(s.rt.DB).Delete(ctx, payer, scope, scopeKey)
}

// ReplaceInvokerSpendLimits fully replaces the payer-owned delegated-spend
// policy document.
func (s *Service) ReplaceInvokerSpendLimits(ctx context.Context, payer identity.CustomerID, next []InvokerSpendLimitInput) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	normalized, err := ValidateInvokerSpendLimitInputs(next)
	if err != nil {
		return err
	}
	rows := make([]admission.InvokerSpendLimit, 0, len(normalized))
	for _, in := range normalized {
		rows = append(rows, invokerSpendLimitRow(in))
	}
	return admission.NewInvokerSpendLimitStore(s.rt.DB).Replace(ctx, payer, rows)
}

// BillingPolicyInput declares one named billing policy (or#897). Window entries
// carry the same {key, window_seconds, limit, currency} shape everywhere in this
// package — SpendLimitWindowInput.
type BillingPolicyInput struct {
	Name string `json:"name"`
	// Kind: outstanding_cap (cap unpaid arrears) or window_spend_cap (cap new
	// spend per window). accrual_rate_cap is refused until or#897 PR 3.
	Kind string `json:"kind"`
	// OutstandingCapAmount (micros, kind=outstanding_cap) is the credit line on
	// unpaid arrears. Zero defers to the payer's own arrears credit limit.
	OutstandingCapAmount int64 `json:"outstanding_cap_amount,omitempty"`
	// SpendWindows (kind=window_spend_cap) are the rolling NEW-spend ceilings.
	SpendWindows []SpendLimitWindowInput `json:"spend_windows,omitempty"`
	// AccrualRateCapPerHour (kind=accrual_rate_cap) is the ceiling on the payer's
	// measured accrual rate, in micros PER HOUR. AccrualRateWindowSeconds is the
	// lookback the measurement smooths over (default 3600); it changes the
	// smoothing, never the unit.
	AccrualRateCapPerHour    int64 `json:"accrual_rate_cap_per_hour,omitempty"`
	AccrualRateWindowSeconds int64 `json:"accrual_rate_window_seconds,omitempty"`
	// CollectionThresholdAmount (micros) is when this payer's accrued arrears is
	// invoiced; DelinquencyGraceDays / DelinquencyAmountFloor are its delinquency
	// policy. Each overrides the merchant-wide invoice setting for payers bound
	// here; nil defers to it. All three ride on any kind.
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
	BadSpendWindows []SpendLimitWindowInput `json:"bad_spend_windows,omitempty"`
	PolicyCurrency  string                  `json:"policy_currency,omitempty"`
}

// BillingPolicyBindingInput points one rung at a policy name (or#897). Set
// CustomerID for the per-customer rung, Tier for the per-tier rung, neither for
// the merchant default — never both.
type BillingPolicyBindingInput struct {
	PolicyName string `json:"policy"`
	CustomerID string `json:"customer_id,omitempty"`
	Tier       string `json:"tier,omitempty"`
}

// DefaultInvokerWastedWindows is the flat delegated-invoker wasted-spend default:
// invokers aren't trusted (an account mints unlimited invokers), so the
// per-invoker budget is a fixed backstop rather than trust-level-graduated. Amounts use
// the request currency's internal precision.
func DefaultInvokerWastedWindows() []abuse.WastedWindow {
	return []abuse.WastedWindow{
		{Key: "burst", Window: 15 * time.Minute, Limit: 5_000_000},
		{Key: "sustained", Window: 5 * time.Hour, Limit: 20_000_000},
	}
}

// MerchantConfiguration is the service-level representation of a merchant's
// one-row configuration payload.
type MerchantConfiguration struct {
	Profile                            *models.MerchantProfileConfiguration
	InvoiceCollectionThreshold         *int64
	InvoiceMonthlyFloor                *int64
	InvoiceBillingBoundary             string
	DelegatedInvokerWastedSpendWindows []abuse.WastedWindow
	// AlertEmail is the merchant-operator alert destination (#736). A nil pointer
	// preserves the stored value; a non-nil pointer sets it (empty string clears).
	AlertEmail *string
	// RepriceNoticeWindowDays (#781) is the merchant-configurable minimum
	// advance-notice window (in days) for a subscription price INCREASE. A
	// nil pointer preserves the stored value; a non-nil pointer sets it (the
	// reprice service falls back to
	// subscriptions.DefaultRepriceNoticeWindowDays when unset).
	RepriceNoticeWindowDays *int
	// ArrearsGraceDays / ArrearsDelinquencyFloor (or#878) are the arrears
	// delinquency policy: how long past due_at a payer keeps grace, and the
	// smallest overdue balance that can escalate. A nil pointer preserves the
	// stored value; unset falls back to delinquency.DefaultGraceDays and to the
	// merchant's InvoiceMonthlyFloor respectively.
	ArrearsGraceDays        *int
	ArrearsDelinquencyFloor *int64
	// CheckoutRouting (or#288) is the processor-routing policy — the mode-2
	// twin of the manifest's checkout_routing block. A nil pointer preserves
	// the stored policy; a non-nil pointer replaces it whole (an empty slice
	// clears it back to the built-in default order).
	CheckoutRouting *[]models.CheckoutRoutingRule
}

// GetMerchantConfiguration returns the stored merchant-scoped configuration row.
func (s *Service) GetMerchantConfiguration(ctx context.Context) (MerchantConfiguration, bool, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return MerchantConfiguration{}, false, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return MerchantConfiguration{}, false, fmt.Errorf("service not initialized")
	}
	cfg, found, err := merchantconfig.NewStore(s.rt.DB).Get(ctx)
	if err != nil {
		return MerchantConfiguration{}, false, err
	}
	alertEmail := cfg.AlertEmail
	// A nil pointer means "no policy declared" — distinct from a pointer to an
	// empty list, which a writer uses to CLEAR one.
	var routing *[]models.CheckoutRoutingRule
	if len(cfg.CheckoutRouting) > 0 {
		rules := cfg.CheckoutRouting
		routing = &rules
	}
	out := MerchantConfiguration{
		Profile:                            &cfg.Profile,
		InvoiceCollectionThreshold:         cfg.InvoiceCollectionThreshold,
		InvoiceMonthlyFloor:                cfg.InvoiceMonthlyFloor,
		InvoiceBillingBoundary:             cfg.InvoiceBillingBoundary,
		AlertEmail:                         &alertEmail,
		RepriceNoticeWindowDays:            cfg.RepriceNoticeWindowDays,
		ArrearsGraceDays:                   cfg.ArrearsGraceDays,
		ArrearsDelinquencyFloor:            cfg.ArrearsDelinquencyFloor,
		CheckoutRouting:                    routing,
		DelegatedInvokerWastedSpendWindows: make([]abuse.WastedWindow, 0, len(cfg.DelegatedInvokerWastedSpendWindows)),
	}
	for _, w := range cfg.DelegatedInvokerWastedSpendWindows {
		if w.WindowSeconds <= 0 {
			continue
		}
		out.DelegatedInvokerWastedSpendWindows = append(out.DelegatedInvokerWastedSpendWindows, abuse.WastedWindow{
			Key:      w.Key,
			Window:   time.Duration(w.WindowSeconds) * time.Second,
			Limit:    w.Limit,
			Currency: w.Currency,
		})
	}
	return out, found, nil
}

// SetMerchantConfiguration persists the merchant-scoped configuration row. An
// empty DelegatedInvokerWastedSpendWindows slice clears that key to the
// DefaultInvokerWastedWindows fallback. A nil Profile preserves the current
// profile; a non-nil empty Profile intentionally clears it.
func (s *Service) SetMerchantConfiguration(ctx context.Context, in MerchantConfiguration) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	cfg, _, err := merchantconfig.NewStore(s.rt.DB).Get(ctx)
	if err != nil {
		return err
	}
	if in.Profile != nil {
		cfg.Profile = *in.Profile
	}
	if in.InvoiceCollectionThreshold != nil {
		if *in.InvoiceCollectionThreshold < 0 {
			return fmt.Errorf("collection_threshold must be >= 0")
		}
		cfg.InvoiceCollectionThreshold = in.InvoiceCollectionThreshold
	}
	if in.InvoiceMonthlyFloor != nil {
		if *in.InvoiceMonthlyFloor < 0 {
			return fmt.Errorf("monthly_floor must be >= 0")
		}
		cfg.InvoiceMonthlyFloor = in.InvoiceMonthlyFloor
	}
	if in.InvoiceBillingBoundary != "" {
		if money.NormalizeInvoiceBoundary(in.InvoiceBillingBoundary) == "" {
			return fmt.Errorf("invalid billing_period_boundary %q", in.InvoiceBillingBoundary)
		}
		cfg.InvoiceBillingBoundary = in.InvoiceBillingBoundary
	}
	if in.AlertEmail != nil {
		cfg.AlertEmail = strings.TrimSpace(*in.AlertEmail)
	}
	if in.RepriceNoticeWindowDays != nil {
		if *in.RepriceNoticeWindowDays < 0 {
			return fmt.Errorf("reprice_notice_window_days must be >= 0")
		}
		cfg.RepriceNoticeWindowDays = in.RepriceNoticeWindowDays
	}
	if in.ArrearsGraceDays != nil {
		if *in.ArrearsGraceDays < 0 {
			return fmt.Errorf("arrears_grace_days must be >= 0")
		}
		cfg.ArrearsGraceDays = in.ArrearsGraceDays
	}
	if in.ArrearsDelinquencyFloor != nil {
		if *in.ArrearsDelinquencyFloor < 0 {
			return fmt.Errorf("arrears_delinquency_floor must be >= 0")
		}
		cfg.ArrearsDelinquencyFloor = in.ArrearsDelinquencyFloor
	}
	if in.CheckoutRouting != nil {
		routing, err := merchantconfig.NormalizeCheckoutRouting(*in.CheckoutRouting)
		if err != nil {
			return err
		}
		cfg.CheckoutRouting = routing
	}
	cfg.DelegatedInvokerWastedSpendWindows = make([]models.BudgetWindowPolicy, 0, len(in.DelegatedInvokerWastedSpendWindows))
	for _, w := range in.DelegatedInvokerWastedSpendWindows {
		if w.Window <= 0 {
			continue
		}
		cfg.DelegatedInvokerWastedSpendWindows = append(cfg.DelegatedInvokerWastedSpendWindows, models.BudgetWindowPolicy{
			Key:           w.Key,
			WindowSeconds: int64(w.Window / time.Second),
			Limit:         w.Limit,
			Currency:      w.Currency,
		})
	}
	return merchantconfig.NewStore(s.rt.DB).Upsert(ctx, cfg)
}

// invokerWastedSpendPolicy resolves the merchant-configured flat delegated
// invoker wasted-spend windows, falling back to DefaultInvokerWastedWindows()
// when no stored config exists.
func (s *Service) invokerWastedSpendPolicy(ctx context.Context) ([]abuse.WastedWindow, error) {
	cfg, _, err := merchantconfig.NewStore(s.rt.DB).Get(ctx)
	if err != nil {
		return nil, err
	}
	if len(cfg.DelegatedInvokerWastedSpendWindows) == 0 {
		return DefaultInvokerWastedWindows(), nil
	}
	out := make([]abuse.WastedWindow, 0, len(cfg.DelegatedInvokerWastedSpendWindows))
	for _, w := range cfg.DelegatedInvokerWastedSpendWindows {
		if w.WindowSeconds <= 0 {
			continue
		}
		out = append(out, abuse.WastedWindow{Key: w.Key, Window: time.Duration(w.WindowSeconds) * time.Second, Limit: w.Limit, Currency: w.Currency})
	}
	if len(out) == 0 {
		return DefaultInvokerWastedWindows(), nil
	}
	return out, nil
}

// payerWastedWindows resolves the PAYER's wasted-spend budget windows from the
// bound billing policy's bad_spend windows (#488) at the payer's current tier.
func (s *Service) payerWastedWindows(ctx context.Context, payer identity.CustomerID, currency, trustLevel string) ([]abuse.WastedWindow, error) {
	if trustLevel == "" {
		if t, err := s.GetTrustLevel(ctx, payer, currency); err == nil && t != "" {
			trustLevel = t
		} else {
			trustLevel = admission.DefaultTrustLevel
		}
	}
	pol, err := admission.NewBillingPolicyStore(s.rt.DB).Resolve(ctx, payer, trustLevel)
	if err != nil {
		return nil, err
	}
	out := make([]abuse.WastedWindow, 0, len(pol.BadSpendWindows))
	for _, w := range pol.BadSpendWindows {
		if w.WindowSeconds <= 0 {
			continue
		}
		out = append(out, abuse.WastedWindow{Key: w.Key, Window: time.Duration(w.WindowSeconds) * time.Second, Limit: w.Limit, Currency: w.Currency})
	}
	return out, nil
}

// WastedSpendInput is one host-reported failed attempt that cost the platform
// money.
//
// Source+SourceID are required (enforced below) and must be REPRODUCIBLE across
// retries of the same failed attempt: together with the engine-composed
// operation "usage:wasted_spend" they ARE the report's identity (or#894 keeps
// that operation distinct so the charge never aliases the CAPTURE of the same
// request id).
//
// ONE guarantee now, not two layers (or#903). Every report writes a durable
// usage_events row under a structural unique key, so the duplicate verdict, the
// no-re-accounting-of-grace property and the money are all the same fact: a
// replay is answered Duplicate with no side effect however long ago the first
// one landed and whatever happened to Redis in between, and a replay whose
// chargeable amount changed is refused with money.ErrIdempotencyKeyReused
// rather than silently dropped. There is no TTL to reason about, and a host
// needs no claim table of its own.
//
// Reported measurements: or#891, or#903, and DESIGN-RULINGS §4.23.
type WastedSpendInput struct {
	CustomerID  identity.CustomerID
	Invoker     string
	InvokerType string
	Currency    string
	Amount      int64
	Source      string
	SourceID    string
	Reason      string
}

// wastedSpendEventType is the metered event kind a charged overage posts under.
const wastedSpendEventType = "wasted_spend"

// WastedSpendResult describes how OpenRails handled one wasted-spend report.
type WastedSpendResult struct {
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

// ReportWastedSpend records host-reported WASTED $ (#497): delegated invokers
// accrue against their flat Redis cutoff, while direct payer credentials accrue
// against trust-level-graduated payer grace and charge overage through the normal
// usage ledger.
//
// # or#903 — the ordering, and why it is this way round
//
// Every report writes ONE durable usage_events row at operation
// "usage:wasted_spend" over (source, source_id), whatever its money outcome:
// the charged overage, a fully forgiven report (amount 0) and a delegated
// invoker's report (amount 0) all land the same row under the same unique
// index. That row IS the once-only claim, and the Redis windows are advanced
// only when it APPLIES. Before or#903 the order was inverted — a SetNX claimed
// the report and the ledger was reached only for a chargeable overage — so the
// claim was a cache: it expired, it did not survive a flush, and a replay after
// a flush was re-graded and re-counted against the payer's grace. It also made
// the engine's own changed-amount refusal unreachable inside the TTL, which is
// why hosts grew claim tables of their own (th#1464's outbox fingerprint).
//
// Consequences worth stating, because they are the contract now:
//
//   - A replay is answered Duplicate=true and consumes no grace, forever, not
//     for a TTL, and across a Redis flush.
//   - A replay whose chargeable amount CHANGED is refused with
//     money.ErrIdempotencyKeyReused instead of being silently dropped.
//   - A crash between the durable row and the counter advance leaves grace
//     UNCONSUMED, never double-consumed: the residual error is in the payer's
//     favour.
//   - The free/delegated path now writes a Postgres row per report. That is a
//     deliberate reversal of "no event table for free reports": a report the
//     platform cannot recognise as already-seen is not free, it is just
//     unaccounted somewhere else.
func (s *Service) ReportWastedSpend(ctx context.Context, in WastedSpendInput) (*WastedSpendResult, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if s.rt.RedisClient == nil {
		return nil, fmt.Errorf("wasted-spend tracking unavailable: redis not configured")
	}
	if in.CustomerID.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if in.Amount < 0 {
		return nil, fmt.Errorf("amount must be >= 0")
	}
	cur, err := requireCurrency(in.Currency)
	if err != nil {
		return nil, err
	}
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return nil, err
	}
	in.Source = strings.TrimSpace(in.Source)
	in.SourceID = strings.TrimSpace(in.SourceID)
	if in.Source == "" || in.SourceID == "" {
		return nil, fmt.Errorf("source and source_id required")
	}
	if in.Amount == 0 {
		return &WastedSpendResult{Currency: cur, Action: "ignored"}, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	lim := ratelimit.NewLimiter(s.rt.RedisClient)
	guard := abuse.NewWastedSpendGuard(lim)
	payerWindows, err := s.payerWastedWindows(ctx, in.CustomerID, cur, "")
	if err != nil {
		return nil, err
	}
	payerPolicyCurrency, err := serviceWastedCurrency(cur, payerWindows)
	if err != nil {
		return nil, err
	}
	invokerWindows, err := s.invokerWastedSpendPolicy(ctx)
	if err != nil {
		return nil, err
	}
	invokerPolicyCurrency, err := serviceWastedCurrency(cur, invokerWindows)
	if err != nil {
		return nil, err
	}
	merchantID := tid.UUID().String()
	payerID := in.CustomerID.UUID().String()

	// or#894: the row posts at operation "usage:wasted_spend", NOT at the bare
	// (source, source_id) the caller reported. Without the operation it aliased
	// the CAPTURE of the same rendered request — the capture then moved 0 micros
	// and returned the waste transfer.
	wasteKey, err := money.NewIdempotencyKey(money.UsageOperation(wastedSpendEventType), in.Source, in.SourceID)
	if err != nil {
		return nil, err
	}

	// THE CLAIM CHECK, ahead of everything that reads or writes a counter.
	// Grading consumes nothing, but it READS the grace window, and the window
	// already contains this report's own first application — so a replay grades
	// to a different chargeable amount than the one on file. Deciding "already
	// seen" from the durable row instead of from the re-derived number is what
	// makes an identical retry a duplicate rather than a spurious conflict.
	if claimed, err := s.claimedWasteReport(ctx, in, cur, wasteKey); err != nil || claimed != nil {
		return claimed, err
	}

	if identity.IsDirectPayerInvoker(in.InvokerType) {
		policyAmount, _, err := fx.ConvertAmount(ctx, s.rt.FXProvider, cur, payerPolicyCurrency, in.Amount)
		if err != nil {
			return nil, err
		}
		// GRADE against grace without consuming it — the durable row below
		// decides whether this report gets to consume anything at all.
		chargeablePolicy, err := guard.PayerGraceOverage(ctx, merchantID, payerID, payerPolicyCurrency, policyAmount, payerWindows)
		if err != nil {
			return nil, err
		}
		chargeable, _, err := fx.ConvertAmount(ctx, s.rt.FXProvider, payerPolicyCurrency, cur, chargeablePolicy)
		if err != nil {
			return nil, err
		}
		if chargeable > in.Amount {
			chargeable = in.Amount
		}
		policyForgiven := policyAmount - chargeablePolicy
		res := &WastedSpendResult{
			Currency:             cur,
			PolicyCurrency:       payerPolicyCurrency,
			RecordedAmount:       in.Amount,
			PolicyRecordedAmount: policyAmount,
			ForgivenAmount:       in.Amount - chargeable,
			PolicyForgivenAmount: policyForgiven,
			ChargedAmount:        chargeable,
			PolicyChargedAmount:  chargeablePolicy,
			Action:               "forgiven",
		}
		// The durable claim. Amount is the overage (often 0) and the ledger is
		// debited only when it is positive, but the ROW is written either way:
		// that is what makes a forgiven report as replay-proof as a charged one.
		ev, err := s.moneyService().RecordUsage(ctx, money.RecordUsageParams{
			Payer:      &in.CustomerID,
			Invoker:    strings.TrimSpace(in.Invoker),
			Currency:   cur,
			EventType:  wastedSpendEventType,
			Amount:     chargeable,
			Key:        wasteKey,
			Dimensions: map[string]int64{wastedReportedDimension: in.Amount},
			Metadata: map[string]any{
				"reason":                   in.Reason,
				"reported_amount":          in.Amount,
				"forgiven_amount":          res.ForgivenAmount,
				"policy_currency":          payerPolicyCurrency,
				"policy_amount":            policyAmount,
				"policy_chargeable_amount": chargeablePolicy,
				"invoker_type":             string(identity.InvokerTypePayer),
				"chargeable_amount":        chargeable,
			},
		})
		if err != nil {
			return s.wasteWriteRaceLost(ctx, in, cur, wasteKey, err)
		}
		if ev.Replayed {
			return &WastedSpendResult{Currency: cur, Action: "duplicate", Duplicate: true}, nil
		}
		if err := guard.ConsumePayerGrace(ctx, merchantID, payerID, payerPolicyCurrency, policyAmount, payerWindows); err != nil {
			return nil, err
		}
		if chargeable > 0 {
			res.Action = "charged"
		}
		return res, nil
	}

	policyAmount, _, err := fx.ConvertAmount(ctx, s.rt.FXProvider, cur, invokerPolicyCurrency, in.Amount)
	if err != nil {
		return nil, err
	}
	// A delegated invoker is never charged, so the durable row carries amount 0.
	// It exists for one reason: to be the thing that says "already seen" when the
	// flat cutoff counter — a cache — cannot.
	ev, err := s.moneyService().RecordUsage(ctx, money.RecordUsageParams{
		Payer:      &in.CustomerID,
		Invoker:    strings.TrimSpace(in.Invoker),
		Currency:   cur,
		EventType:  wastedSpendEventType,
		Amount:     0,
		Key:        wasteKey,
		Dimensions: map[string]int64{wastedReportedDimension: in.Amount},
		Metadata: map[string]any{
			"reason":          in.Reason,
			"reported_amount": in.Amount,
			"policy_currency": invokerPolicyCurrency,
			"policy_amount":   policyAmount,
			"invoker_type":    strings.TrimSpace(in.InvokerType),
		},
	})
	if err != nil {
		return s.wasteWriteRaceLost(ctx, in, cur, wasteKey, err)
	}
	if ev.Replayed {
		return &WastedSpendResult{Currency: cur, Action: "duplicate", Duplicate: true}, nil
	}
	if err := guard.RecordInvokerCutoff(ctx, merchantID, payerID, in.Invoker, invokerPolicyCurrency, policyAmount, invokerWindows); err != nil {
		return nil, err
	}
	return &WastedSpendResult{Currency: cur, PolicyCurrency: invokerPolicyCurrency, RecordedAmount: in.Amount, PolicyRecordedAmount: policyAmount, Action: "invoker_cutoff_tracked"}, nil
}

// wastedReportedDimension carries the REPORTED wasted amount on the durable
// row. It is a typed dimension rather than a metadata entry because it is
// compared on every replay: metadata round-trips through JSONB as float64, and
// a money comparison must not go anywhere near a float.
const wastedReportedDimension = "reported_amount"

// claimedWasteReport answers whether this report's key is already claimed.
//
//   - unclaimed            -> (nil, nil), the caller proceeds
//   - claimed, same body   -> the duplicate verdict, no side effect
//   - claimed, CHANGED body-> money.ErrIdempotencyKeyReused
//
// The comparison is on the REPORTED amount, not on what was charged: two
// different reports can both be fully forgiven, and answering the second with
// "duplicate" would silently drop a real number. This is the refusal a host
// used to have to build itself out of a body fingerprint.
func (s *Service) claimedWasteReport(ctx context.Context, in WastedSpendInput, cur string, key money.IdempotencyKey) (*WastedSpendResult, error) {
	ev, err := s.moneyService().FindUsageEvent(ctx, in.CustomerID, cur, wastedSpendEventType, key)
	if err != nil || ev == nil {
		return nil, err
	}
	if committed, ok := ev.Dimensions[wastedReportedDimension]; ok && committed != in.Amount {
		return nil, &money.IdempotencyConflict{
			Operation: string(key.Operation()), Source: key.Source(), SourceID: key.SourceID(),
			Field: wastedReportedDimension, Committed: committed, Retried: in.Amount,
		}
	}
	return &WastedSpendResult{Currency: cur, Action: "duplicate", Duplicate: true}, nil
}

// wasteWriteRaceLost interprets a refusal from the durable write. Two identical
// reports racing each other grade against the same window, so they normally
// agree and the loser is answered Replayed; but if the winner's ConsumePayerGrace
// lands between the loser's grading and its write, the loser computes a larger
// overage and is refused for an amount that is not actually a changed body. Only
// the durable row can tell those apart, so ask it.
func (s *Service) wasteWriteRaceLost(ctx context.Context, in WastedSpendInput, cur string, key money.IdempotencyKey, cause error) (*WastedSpendResult, error) {
	if !errors.Is(cause, money.ErrIdempotencyKeyReused) {
		return nil, cause
	}
	claimed, err := s.claimedWasteReport(ctx, in, cur, key)
	if err != nil || claimed == nil {
		return nil, cause
	}
	return claimed, nil
}

func serviceWastedCurrency(requestCurrency string, windows []abuse.WastedWindow) (string, error) {
	cur := money.NormalizeCurrency(requestCurrency)
	explicit := false
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return "", err
	}
	for _, w := range windows {
		if strings.TrimSpace(w.Currency) == "" {
			continue
		}
		wc := money.NormalizeCurrency(w.Currency)
		if err := moneyutil.ValidateCurrency(wc); err != nil {
			return "", err
		}
		if !explicit {
			cur = wc
			explicit = true
			continue
		}
		if cur != wc {
			return "", fmt.Errorf("mixed wasted-spend currencies are not supported: %s and %s", cur, wc)
		}
	}
	return cur, nil
}

// ErrInvalidBillingPolicy identifies caller-owned billing-policy input errors so
// HTTP and embedded transports map them to the same 400/ErrInvalid contract.
var ErrInvalidBillingPolicy = errors.New("invalid billing policy")

// ValidateBillingPolicy runs the ONE shared normalizer (or#288 pattern) over a
// declared policy. Both the manifest loader and this API call it, so a policy
// that boots cannot be one the API would have refused.
func ValidateBillingPolicy(in BillingPolicyInput) (string, models.BillingPolicy, error) {
	name, err := merchantconfig.NormalizeBillingPolicyName(in.Name)
	if err != nil {
		return "", models.BillingPolicy{}, fmt.Errorf("%w: %s", ErrInvalidBillingPolicy, err)
	}
	body, err := merchantconfig.NormalizeBillingPolicy(name, models.BillingPolicy{
		Kind:                      models.BillingPolicyKind(in.Kind),
		OutstandingCapAmount:      in.OutstandingCapAmount,
		SpendWindows:              budgetScopeWindowModels(in.SpendWindows),
		AccrualRateCapPerHour:     in.AccrualRateCapPerHour,
		AccrualRateWindowSeconds:  in.AccrualRateWindowSeconds,
		BadSpendWindows:           budgetScopeWindowModels(in.BadSpendWindows),
		CollectionThresholdAmount: in.CollectionThresholdAmount,
		CollectionCycleBoundary:   in.CollectionCycleBoundary,
		DelinquencyGraceDays:      in.DelinquencyGraceDays,
		DelinquencyAmountFloor:    in.DelinquencyAmountFloor,
		PolicyCurrency:            in.PolicyCurrency,
	})
	if err != nil {
		return "", models.BillingPolicy{}, fmt.Errorf("%w: %s", ErrInvalidBillingPolicy, err)
	}
	return name, body, nil
}

// SetBillingPolicy declares (or redeclares) one named billing policy (or#897).
// Declaring a policy binds nothing — BindBillingPolicy decides who gets it.
func (s *Service) SetBillingPolicy(ctx context.Context, in BillingPolicyInput) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	name, body, err := ValidateBillingPolicy(in)
	if err != nil {
		return err
	}
	if err := admission.NewBillingPolicyStore(s.rt.DB).UpsertPolicy(ctx, name, body); err != nil {
		return err
	}
	s.invalidateAdmissionPolicyCache(ctx)
	return nil
}

// BindBillingPolicy points one rung (customer / tier / merchant default) at a
// declared policy name. This is the merchant's runtime lever: rebinding changes
// which cap applies to a payer and moves no money.
func (s *Service) BindBillingPolicy(ctx context.Context, in BillingPolicyBindingInput) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	customerRaw := strings.TrimSpace(in.CustomerID)
	name, tier, _, err := merchantconfig.NormalizeBillingPolicyBinding(in.PolicyName, in.Tier, customerRaw != "")
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidBillingPolicy, err)
	}
	var payer identity.CustomerID
	if customerRaw != "" {
		id, perr := uuid.Parse(customerRaw)
		if perr != nil {
			return fmt.Errorf("%w: customer_id must be a uuid", ErrInvalidBillingPolicy)
		}
		payer = identity.CustomerID(id)
	}
	if err := admission.NewBillingPolicyStore(s.rt.DB).BindPolicy(ctx, payer, tier, name); err != nil {
		return err
	}
	s.invalidateAdmissionPolicyCache(ctx)
	return nil
}

// ListBillingPolicies returns every declared policy for the config-sync document.
func (s *Service) ListBillingPolicies(ctx context.Context) ([]BillingPolicyInput, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	stored, err := admission.NewBillingPolicyStore(s.rt.DB).ListPolicies(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(stored))
	for name := range stored {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]BillingPolicyInput, 0, len(names))
	for _, name := range names {
		body := stored[name]
		out = append(out, BillingPolicyInput{
			Name:                      name,
			Kind:                      string(body.Kind),
			OutstandingCapAmount:      body.OutstandingCapAmount,
			SpendWindows:              spendLimitWindowInputs(body.SpendWindows),
			AccrualRateCapPerHour:     body.AccrualRateCapPerHour,
			AccrualRateWindowSeconds:  body.AccrualRateWindowSeconds,
			BadSpendWindows:           spendLimitWindowInputs(body.BadSpendWindows),
			CollectionThresholdAmount: body.CollectionThresholdAmount,
			DelinquencyGraceDays:      body.DelinquencyGraceDays,
			DelinquencyAmountFloor:    body.DelinquencyAmountFloor,
			PolicyCurrency:            body.PolicyCurrency,
		})
	}
	return out, nil
}

// ListBillingPolicyBindings returns the DECLARATIVE bindings — the merchant
// default and the per-tier rungs. Per-customer bindings are runtime segmentation
// state and are never enumerated (that read would scale with customers).
func (s *Service) ListBillingPolicyBindings(ctx context.Context) ([]BillingPolicyBindingInput, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	rows, err := admission.NewBillingPolicyStore(s.rt.DB).ListDeclarativeBindings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]BillingPolicyBindingInput, 0, len(rows))
	for _, r := range rows {
		b := BillingPolicyBindingInput{PolicyName: r.PolicyName, Tier: r.Tier}
		if r.CustomerID != nil {
			b.CustomerID = r.CustomerID.String()
		}
		out = append(out, b)
	}
	return out, nil
}

// invalidateAdmissionPolicyCache retires this process's cached resolutions for
// the merchant after a policy or binding write, so a tightened cap takes effect
// on the next admit instead of at the end of the TTL.
func (s *Service) invalidateAdmissionPolicyCache(ctx context.Context) {
	if s.rt.AdmissionPolicyCache == nil {
		return
	}
	if tid, err := merchant.Require(ctx); err == nil {
		s.rt.AdmissionPolicyCache.InvalidateMerchant(tid.UUID().String())
	}
}

// TrustLevelScheduleRung is one rung of a persisted same-currency trust-level
// ladder (#476): a payer reaches TrustLevel once its cumulative paid spend in
// the schedule currency is at least MinCumulativePaidAmount.
type TrustLevelScheduleRung struct {
	TrustLevel              string `json:"trust_level"`
	MinCumulativePaidAmount int64  `json:"min_cumulative_paid_amount"`
}

// SetTrustLevelSchedule persists the merchant's trust-level SCHEDULE (#476): the
// host declares the same-currency ladder ONCE and OpenRails then AUTO-maintains
// each payer's trust level from cumulative spend (no host cranking). A zero
// payer sets the merchant-wide default schedule; a non-zero payer sets a
// per-subject override. owner=platform.
func (s *Service) SetTrustLevelSchedule(ctx context.Context, payer identity.CustomerID, currency string, schedule []TrustLevelScheduleRung) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return err
	}
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return err
	}
	moneySvc := money.NewMoneyService(s.rt.DB)
	rungs := make([]money.TrustLevelThreshold, 0, len(schedule))
	for _, r := range schedule {
		rungs = append(rungs, money.TrustLevelThreshold{TrustLevel: r.TrustLevel, MinPaidAmount: r.MinCumulativePaidAmount})
	}
	return moneySvc.SetTrustLevelSchedule(ctx, payer, cur, rungs)
}

// GetTrustLevel returns the payer's current trust level (#477) for one currency:
// the value OpenRails auto-maintains from same-currency cumulative paid spend
// against the persisted trust-level schedule (#476), or a manual admin override.
// Empty means the caller treats it as the lowest/default trust level.
func (s *Service) GetTrustLevel(ctx context.Context, payer identity.CustomerID, currency string) (string, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return "", pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return "", fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return "", fmt.Errorf("payer required")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return "", err
	}
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return "", err
	}
	return money.NewMoneyService(s.rt.DB).GetTrustLevel(ctx, payer, cur)
}
