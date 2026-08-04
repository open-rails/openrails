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
	Source          string
	SourceID        string
	ExpiresAtUnix   int64
}

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
// stateless). #513: no Postgres locks, no per-request budget reservation rows.
func (s *Service) Admit(ctx context.Context, in AdmitInput) (*AdmitResult, error) {
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
		admission.NewPayerSpendLimitStore(s.rt.DB),
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
		WithDelinquency(s.delinquencyService())

	var exp time.Time
	switch {
	case in.ExpiresAtUnix > 0:
		exp = time.Unix(in.ExpiresAtUnix, 0).UTC()
	case in.EstimatedAmount > 0:
		exp = s.now().Add(time.Hour)
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
		Source:          source,
		SourceID:        in.SourceID,
		ExpiresAt:       exp,
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

// SpendLimitWindowInput is one fixed money-budget window for a budget-scope
// policy (#473): same shape as TrustLevelBudgetWindowInput.
type SpendLimitWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}

// InvokerSpendLimitInput configures one hierarchical budget-scope policy (#473).
// Scope is "subject" | "role" | "invoker" | "invoker_tier"; ScopeKey is the
// role uuid, invoker string, or invoker-tier key, empty for scope=subject.
type InvokerSpendLimitInput struct {
	Scope    string                  `json:"scope"`
	ScopeKey string                  `json:"scope_key,omitempty"`
	RoleID   string                  `json:"role_id,omitempty"`
	Windows  []SpendLimitWindowInput `json:"windows"`
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
// scope, scope_key, and role_id aliases are normalized, so every transport has
// identical replacement semantics.
func ValidateInvokerSpendLimitInputs(in []InvokerSpendLimitInput) ([]InvokerSpendLimitInput, error) {
	out := make([]InvokerSpendLimitInput, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, item := range in {
		scope := budgets.NormalizeScope(item.Scope)
		scopeKey := strings.TrimSpace(item.ScopeKey)
		roleID := strings.TrimSpace(item.RoleID)
		if roleID != "" && scope != budgets.ScopeRole {
			return nil, invalidInvokerSpendLimit(fmt.Sprintf("delegations[%d].role_id is only allowed for scope %q", i, budgets.ScopeRole))
		}
		if scopeKey != "" && roleID != "" && scopeKey != roleID {
			return nil, invalidInvokerSpendLimit(fmt.Sprintf("delegations[%d].role_id must match scope_key", i))
		}
		if scopeKey == "" {
			scopeKey = roleID
		}
		row, err := admission.ValidateInvokerSpendLimit(admission.InvokerSpendLimit{
			Scope: scope, ScopeKey: scopeKey, Windows: budgetScopeWindowModels(item.Windows),
		})
		if err != nil {
			return nil, invalidInvokerSpendLimit(fmt.Sprintf("delegations[%d].%s", i, err))
		}
		key := invokerSpendLimitKey(row.Scope, row.ScopeKey)
		if _, duplicate := seen[key]; duplicate {
			return nil, invalidInvokerSpendLimit(fmt.Sprintf("duplicate delegation for %s", key))
		}
		seen[key] = struct{}{}
		normalized := InvokerSpendLimitInput{
			Scope: row.Scope, ScopeKey: row.ScopeKey, Windows: spendLimitWindowInputs(row.Windows),
		}
		if row.Scope == budgets.ScopeRole {
			normalized.RoleID = row.ScopeKey
		}
		out = append(out, normalized)
	}
	sort.Slice(out, func(i, j int) bool {
		return invokerSpendLimitKey(out[i].Scope, out[i].ScopeKey) < invokerSpendLimitKey(out[j].Scope, out[j].ScopeKey)
	})
	return out, nil
}

func invokerSpendLimitRow(in InvokerSpendLimitInput) admission.InvokerSpendLimit {
	return admission.InvokerSpendLimit{
		Scope: in.Scope, ScopeKey: in.ScopeKey, Windows: budgetScopeWindowModels(in.Windows),
	}
}

// SetInvokerSpendLimits upserts a SUBJECT-owned budget-scope policy (#473): the
// subject's self cap, a role pool, an invoker grant, or an invoker-tier grant.
// Payer-set: the payer caps how much its delegated invokers/roles may spend.
func (s *Service) SetInvokerSpendLimits(ctx context.Context, payer identity.CustomerID, in InvokerSpendLimitInput) error {
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

// InvokerSpendLimits returns the payer's per-invoker spend limits (#473/#517).
func (s *Service) InvokerSpendLimits(ctx context.Context, payer identity.CustomerID) ([]InvokerSpendLimitInput, error) {
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
		out = append(out, InvokerSpendLimitInput{Scope: r.Scope, ScopeKey: r.ScopeKey, Windows: w})
	}
	return out, nil
}

// ReplaceInvokerSpendLimits fully replaces the payer-owned delegated-spend
// policy document.
func (s *Service) ReplaceInvokerSpendLimits(ctx context.Context, payer identity.CustomerID, next []InvokerSpendLimitInput) error {
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

// TrustLevelBudgetWindowInput / PayerSpendLimitInput configure a trust level's
// money policy via the admin endpoint (#298: trust-level admin API).
type TrustLevelBudgetWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}

type PayerSpendLimitInput struct {
	TrustLevel     string                        `json:"trust_level,omitempty"`
	BudgetWindows  []TrustLevelBudgetWindowInput `json:"budget_windows"`
	PolicyCurrency string                        `json:"policy_currency,omitempty"`
	// BadSpendWindows are the #497 per-PAYER direct-credential wasted-spend grace
	// windows for this trust level: at most Limit of host-reported wasted spend is
	// forgiven per window; direct-payer overage is charged.
	BadSpendWindows []TrustLevelBudgetWindowInput `json:"bad_spend_windows,omitempty"`
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
// trust-level policy's bad_spend windows (#488) at the payer's current trust level.
func (s *Service) payerWastedWindows(ctx context.Context, payer identity.CustomerID, currency, trustLevel string) ([]abuse.WastedWindow, error) {
	if trustLevel == "" {
		if t, err := s.GetTrustLevel(ctx, payer, currency); err == nil && t != "" {
			trustLevel = t
		} else {
			trustLevel = admission.DefaultTrustLevel
		}
	}
	pol, err := admission.NewPayerSpendLimitStore(s.rt.DB).GetPayerSpendLimits(ctx, payer, trustLevel)
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
// retries of the same failed attempt. Read the guarantee precisely, because the
// two layers differ:
//
//   - The DUPLICATE VERDICT (Duplicate=true, no grace re-accounting) is a Redis
//     SetNX in abuse.WastedSpendGuard.ClaimReport. It is a cache, not a durable
//     claim: it expires with the widest configured window and does not survive a
//     flush. After a flush a replay is re-graded and re-counted against grace.
//   - The MONEY is durable regardless: the direct-payer overage charge posts
//     through money.RecordUsage at operation "usage:wasted_spend" over
//     (source, source_id), whose key is structural, so a replay cannot
//     double-charge and a replay with a changed amount is refused
//     (money.ErrIdempotencyKeyReused). The operation is engine-composed, so the
//     charge never aliases the CAPTURE of the same request id (or#894).
//
// Reported measurements: or#891, and DESIGN-RULINGS §4.23.
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
// against trust-level-graduated payer grace and charge overage through the normal usage
// ledger. No high-volume Postgres event table is written for free/delegated
// reports.
func (s *Service) ReportWastedSpend(ctx context.Context, in WastedSpendInput) (*WastedSpendResult, error) {
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
	claimed, err := guard.ClaimReport(ctx, tid.UUID().String(), in.CustomerID.UUID().String(), cur, in.Source, in.SourceID, maxWastedWindowTTL(payerWindows, invokerWindows))
	if err != nil {
		return nil, err
	}
	if !claimed {
		return &WastedSpendResult{Currency: cur, Action: "duplicate", Duplicate: true}, nil
	}

	if identity.IsDirectPayerInvoker(in.InvokerType) {
		policyAmount, _, err := fx.ConvertAmount(ctx, s.rt.FXProvider, cur, payerPolicyCurrency, in.Amount)
		if err != nil {
			return nil, err
		}
		chargeablePolicy, err := guard.RecordPayerGrace(ctx, tid.UUID().String(), in.CustomerID.UUID().String(), payerPolicyCurrency, policyAmount, payerWindows)
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
		if chargeable > 0 {
			// or#894: the overage charge posts at operation "usage:wasted_spend",
			// NOT at the bare (source, source_id) the caller reported. Without the
			// operation it aliased the CAPTURE of the same rendered request — the
			// capture then moved 0 micros and returned the waste transfer.
			wasteKey, kerr := money.NewIdempotencyKey(money.UsageOperation(wastedSpendEventType), in.Source, in.SourceID)
			if kerr != nil {
				return nil, kerr
			}
			_, err = s.moneyService().RecordUsage(ctx, money.RecordUsageParams{
				Payer:     &in.CustomerID,
				Invoker:   strings.TrimSpace(in.Invoker),
				Currency:  cur,
				EventType: wastedSpendEventType,
				Amount:    chargeable,
				Key:       wasteKey,
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
				return nil, err
			}
			res.Action = "charged"
		}
		return res, nil
	}

	policyAmount, _, err := fx.ConvertAmount(ctx, s.rt.FXProvider, cur, invokerPolicyCurrency, in.Amount)
	if err != nil {
		return nil, err
	}
	if err := guard.RecordInvokerCutoff(ctx, tid.UUID().String(), in.CustomerID.UUID().String(), in.Invoker, invokerPolicyCurrency, policyAmount, invokerWindows); err != nil {
		return nil, err
	}
	return &WastedSpendResult{Currency: cur, PolicyCurrency: invokerPolicyCurrency, RecordedAmount: in.Amount, PolicyRecordedAmount: policyAmount, Action: "invoker_cutoff_tracked"}, nil
}

func maxWastedWindowTTL(groups ...[]abuse.WastedWindow) time.Duration {
	var max time.Duration
	for _, group := range groups {
		for _, w := range group {
			if w.Window > max {
				max = w.Window
			}
		}
	}
	if max <= 0 {
		return time.Hour
	}
	return max + time.Second
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

// SetPayerSpendLimits upserts a per-payer trust-level money policy.
func (s *Service) SetPayerSpendLimits(ctx context.Context, payer identity.CustomerID, in PayerSpendLimitInput) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	// A ZERO payer writes the TENANT-WIDE DEFAULT trust-level policy (#477): the
	// platform capacity ladder declared once, applied to every payer at the trust level. A non-zero
	// payer writes a per-subject override.
	trustLevel := strings.TrimSpace(in.TrustLevel)
	if trustLevel == "" {
		return fmt.Errorf("trust_level required")
	}
	pol := models.TrustLevelMoneyPolicy{PolicyCurrency: in.PolicyCurrency}
	for _, b := range in.BudgetWindows {
		pol.BudgetWindows = append(pol.BudgetWindows, models.BudgetWindowPolicy{Key: b.Key, WindowSeconds: b.WindowSeconds, Limit: b.Limit, Currency: b.Currency})
	}
	for _, b := range in.BadSpendWindows {
		pol.BadSpendWindows = append(pol.BadSpendWindows, models.BudgetWindowPolicy{Key: b.Key, WindowSeconds: b.WindowSeconds, Limit: b.Limit, Currency: b.Currency})
	}
	return admission.NewPayerSpendLimitStore(s.rt.DB).UpsertPayerSpendLimitsFull(ctx, payer, trustLevel, pol)
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
