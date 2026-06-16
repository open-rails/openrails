package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/holds"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AdmitInput is the host's admission request for payer capacity and delegated
// spend gates.
type AdmitInput struct {
	CustomerID  identity.CustomerID
	Invoker     string
	InvokerType string
	Tier        string
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
	PolicyCurrency      string     `json:"policy_currency,omitempty"`
	PolicyAmount        int64      `json:"policy_amount,omitempty"`
	StartCapacityAmount int64      `json:"start_capacity_amount,omitempty"`
	BlockedBy           string     `json:"blocked_by,omitempty"`
	DenyCode            string     `json:"deny_code,omitempty"`
	RetryAfterSeconds   int64      `json:"retry_after_seconds,omitempty"`
	HoldExpiresAt       *time.Time `json:"hold_expires_at,omitempty"`
	// Budget (#304): the rolling money-budget reservation + per-window state.
	BudgetReservationID string                 `json:"budget_reservation_id,omitempty"`
	BudgetWindows       []AdmitBudgetWindowDTO `json:"budget_windows,omitempty"`
}

// AdmitBudgetWindowDTO is a rolling money-budget window's state (#304), for the
// host's /status dashboard.
type AdmitBudgetWindowDTO struct {
	Key               string `json:"key"`
	Currency          string `json:"currency"`
	Limit             int64  `json:"limit"`
	Used              int64  `json:"used"`
	Reserved          int64  `json:"reserved"`
	Remaining         int64  `json:"remaining"`
	ResetAfterSeconds int64  `json:"reset_after_seconds"`
	// ResetAt is the exact window boundary (#337 fixed windows) — displayable
	// as "your next reset is 4:30pm". Zero when the engine predates state.
	ResetAt time.Time `json:"reset_at,omitzero"`
	Cadence string    `json:"cadence,omitempty"`
	Allowed bool      `json:"allowed"`
}

// Admit runs service admission: delegated spend policy + wasted-spend cutoff +
// money capacity + Redis hold. It builds the admitter from the runtime per call.
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

	store := admission.NewTierPolicyStore(s.rt.DB)
	bsvc := budgets.NewService(s.rt.DB)
	lim := ratelimit.NewLimiter(s.rt.RedisClient)
	// Resolve the merchant-configured flat delegated-invoker wasted-spend windows,
	// falling back to DefaultInvokerWastedWindows() when unset.
	invokerWindows, err := s.invokerWastedSpendPolicy(ctx)
	if err != nil {
		return nil, err
	}
	adm := admission.NewAdmitter(s.moneyService(), store, bsvc).
		WithBudgetScopes(admission.NewBudgetPolicyStore(s.rt.DB)).
		WithFXProvider(s.rt.FXProvider).
		WithWastedSpend(abuse.NewWastedSpendGuard(lim), invokerWindows)

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
		Tier:            in.Tier,
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
		PolicyCurrency:      dec.PolicyCurrency,
		PolicyAmount:        dec.PolicyAmount,
		StartCapacityAmount: dec.AccountCapacityAmount,
		BlockedBy:           dec.BlockedBy,
		DenyCode:            dec.DenyCode,
		RetryAfterSeconds:   int64(dec.RetryAfter / time.Second),
	}
	if !dec.Allowed && dec.BlockedBy == "money" && in.EstimatedAmount > 0 {
		mid, err := serviceMerchantID(ctx)
		if err != nil {
			return nil, err
		}
		activeHeld, err := holds.NewStore(s.rt.RedisClient).ActiveAmount(ctx, mid, in.CustomerID.UUID().String(), currency)
		if err != nil {
			return nil, err
		}
		res.StartCapacityAmount = startCapacity(dec.AccountCapacityAmount, activeHeld)
	}
	if dec.Allowed && in.EstimatedAmount > 0 {
		mid, err := serviceMerchantID(ctx)
		if err != nil {
			return nil, err
		}
		store := holds.NewStore(s.rt.RedisClient)
		allowed, activeHeld, err := store.Place(ctx, holds.Hold{
			MerchantID:      mid,
			RequestID:       in.SourceID,
			CustomerID:      in.CustomerID.UUID().String(),
			Invoker:         strings.TrimSpace(in.Invoker),
			InvokerType:     strings.TrimSpace(in.InvokerType),
			Currency:        currency,
			EstimatedAmount: in.EstimatedAmount,
			Source:          source,
			Resource:        strings.TrimSpace(in.Resource),
			PolicyCurrency:  dec.PolicyCurrency,
			PolicyAmount:    dec.PolicyAmount,
			CreatedAt:       s.now().UTC(),
			ExpiresAt:       exp,
		}, dec.AccountCapacityAmount)
		if err != nil {
			s.releaseBudgetScopesByCoords(ctx, in.CustomerID, source, in.SourceID)
			return nil, err
		}
		if !allowed {
			s.releaseBudgetScopesByCoords(ctx, in.CustomerID, source, in.SourceID)
			return &AdmitResult{
				Allowed: false, Currency: currency, EstimatedAmount: in.EstimatedAmount,
				PolicyCurrency: dec.PolicyCurrency, PolicyAmount: dec.PolicyAmount, StartCapacityAmount: startCapacity(dec.AccountCapacityAmount, activeHeld),
				BlockedBy: "money", DenyCode: money.DenyInsufficientBalance,
			}, nil
		}
		res.StartCapacityAmount = startCapacity(dec.AccountCapacityAmount, activeHeld-in.EstimatedAmount)
		res.HoldExpiresAt = &exp
	}
	if dec.BudgetReservationID != uuid.Nil {
		res.BudgetReservationID = dec.BudgetReservationID.String()
	}
	for _, w := range dec.BudgetWindows {
		rs := int64(time.Until(w.ResetAt) / time.Second)
		if rs < 0 {
			rs = 0
		}
		res.BudgetWindows = append(res.BudgetWindows, AdmitBudgetWindowDTO{
			Key: w.Key, Currency: dec.PolicyCurrency, Limit: w.Limit, Used: w.Used, Reserved: w.Reserved,
			Remaining: w.Remaining, ResetAfterSeconds: rs, Allowed: w.Allowed,
		})
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

// BudgetStatus returns the rolling money-budget windows for (payer, invoker) at a
// tier WITHOUT reserving (issue #304 introspection) — powers a host's /status
// dashboard (e.g. cozy-art useGenerationBudgetStatus).
func (s *Service) BudgetStatus(ctx context.Context, payer identity.CustomerID, invoker, currency, tier string) ([]AdmitBudgetWindowDTO, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	if tier == "" {
		tier = admission.DefaultTier
	}
	store := admission.NewTierPolicyStore(s.rt.DB)
	pol, err := store.GetTierPolicy(ctx, payer, tier)
	if err != nil {
		return nil, err
	}
	policyCurrency, err := servicePolicyCurrency(cur, pol.PolicyCurrency, pol.BudgetWindows)
	if err != nil {
		return nil, err
	}
	bsvc := budgets.NewService(s.rt.DB)
	statuses, _, err := bsvc.Check(ctx, payer, invoker, policyCurrency, pol.BudgetWindows, 0)
	if err != nil {
		return nil, err
	}
	out := make([]AdmitBudgetWindowDTO, 0, len(statuses))
	for _, w := range statuses {
		rs := int64(time.Until(w.ResetAt) / time.Second)
		if rs < 0 {
			rs = 0
		}
		out = append(out, AdmitBudgetWindowDTO{
			Key: w.Key, Currency: policyCurrency, Limit: w.Limit, Used: w.Used, Reserved: w.Reserved,
			Remaining: w.Remaining, ResetAfterSeconds: rs, ResetAt: w.ResetAt,
			Cadence: w.Cadence, Allowed: w.Allowed,
		})
	}
	return out, nil
}

// BudgetCheckWindowInput is a caller-supplied fixed budget window for
// BudgetCheck (the host owns the budget policy; OpenRails owns the actuals).
type BudgetCheckWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
	// Cadence is "session" (default) or "fixed" (#337 fixed windows).
	Cadence string `json:"cadence,omitempty"`
}

// BudgetCheck computes per-window used/reserved/remaining for (payer, invoker)
// against CALLER-SUPPLIED windows WITHOUT reserving. Unlike BudgetStatus it does
// NOT read an OpenRails tier policy — the host (tensorhub) owns the platform
// budget policy and passes the windows; OpenRails owns the spend actuals
// (openrails.budget_inflight_holds). Powers the tensorhub delegated budget-window
// display (#410) so it no longer reads tensorhub-local money tables.
func (s *Service) BudgetCheck(ctx context.Context, payer identity.CustomerID, invoker, currency string, windows []BudgetCheckWindowInput, requestedAmount int64) ([]AdmitBudgetWindowDTO, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	bw := make([]budgets.BudgetWindow, 0, len(windows))
	for _, w := range windows {
		bw = append(bw, budgets.BudgetWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency, Cadence: w.Cadence})
	}
	policyCurrency, err := servicePolicyCurrency(cur, "", bw)
	if err != nil {
		return nil, err
	}
	policyAmount, _, err := fx.ConvertAmount(ctx, s.rt.FXProvider, cur, policyCurrency, requestedAmount)
	if err != nil {
		return nil, err
	}
	bsvc := budgets.NewService(s.rt.DB)
	statuses, _, err := bsvc.Check(ctx, payer, invoker, policyCurrency, bw, policyAmount)
	if err != nil {
		return nil, err
	}
	out := make([]AdmitBudgetWindowDTO, 0, len(statuses))
	for _, w := range statuses {
		rs := int64(time.Until(w.ResetAt) / time.Second)
		if rs < 0 {
			rs = 0
		}
		out = append(out, AdmitBudgetWindowDTO{
			Key: w.Key, Currency: policyCurrency, Limit: w.Limit, Used: w.Used, Reserved: w.Reserved,
			Remaining: w.Remaining, ResetAfterSeconds: rs, ResetAt: w.ResetAt,
			Cadence: w.Cadence, Allowed: w.Allowed,
		})
	}
	return out, nil
}

func servicePolicyCurrency(requestCurrency, configured string, windows []budgets.BudgetWindow) (string, error) {
	cur := money.NormalizeCurrency(requestCurrency)
	explicit := false
	if strings.TrimSpace(configured) != "" {
		cur = money.NormalizeCurrency(configured)
		explicit = true
	}
	if err := money.ValidateCurrency(cur); err != nil {
		return "", err
	}
	for _, w := range windows {
		if strings.TrimSpace(w.Currency) == "" {
			continue
		}
		wc := money.NormalizeCurrency(w.Currency)
		if err := money.ValidateCurrency(wc); err != nil {
			return "", err
		}
		if !explicit {
			cur = wc
			explicit = true
			continue
		}
		if cur != wc {
			return "", fmt.Errorf("mixed policy currencies are not supported: %s and %s", cur, wc)
		}
	}
	return cur, nil
}

// BudgetScopeWindowInput is one fixed money-budget window for a budget-scope
// policy (#473): same shape as TierBudgetWindowInput.
type BudgetScopeWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
	Cadence       string `json:"cadence,omitempty"`
}

// BudgetScopePolicyInput configures one hierarchical budget-scope policy (#473).
// Scope is "subject" | "role" | "invoker" | "invoker_tier"; ScopeKey is the
// role uuid, invoker string, or invoker-tier key, empty for scope=subject.
type BudgetScopePolicyInput struct {
	Scope    string                   `json:"scope"`
	ScopeKey string                   `json:"scope_key,omitempty"`
	Windows  []BudgetScopeWindowInput `json:"windows"`
}

func budgetScopeWindowModels(ws []BudgetScopeWindowInput) []models.BudgetWindowPolicy {
	out := make([]models.BudgetWindowPolicy, 0, len(ws))
	for _, w := range ws {
		out = append(out, models.BudgetWindowPolicy{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency, Cadence: w.Cadence})
	}
	return out
}

// SetSubjectBudgetPolicy upserts a SUBJECT-owned budget-scope policy (#473): the
// subject's self cap, a role pool, an invoker grant, or an invoker-tier grant.
// The owner is forced to "subject" — this path can NEVER write a
// platform-owned cap, so a subject cannot create/loosen one.
func (s *Service) SetSubjectBudgetPolicy(ctx context.Context, payer identity.CustomerID, in BudgetScopePolicyInput) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	return admission.NewBudgetPolicyStore(s.rt.DB).Upsert(ctx, payer, admission.BudgetScopePolicy{
		Scope: in.Scope, Owner: "subject", ScopeKey: in.ScopeKey,
		Windows: budgetScopeWindowModels(in.Windows),
	})
}

// SetPlatformBudgetPolicy upserts a PLATFORM-owned budget cap (#473) — the
// platform->payer cap (level 1; tensorhub #475 scope B). Callable ONLY from a
// platform-admin path (the caller enforces operator authz before invoking this);
// the subject's own policy surface can never reach it, and the subject's
// SubjectBudgetPolicies read does not expose it.
func (s *Service) SetPlatformBudgetPolicy(ctx context.Context, payer identity.CustomerID, in BudgetScopePolicyInput) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	return admission.NewBudgetPolicyStore(s.rt.DB).Upsert(ctx, payer, admission.BudgetScopePolicy{
		Scope: in.Scope, Owner: "platform", ScopeKey: in.ScopeKey,
		Windows: budgetScopeWindowModels(in.Windows),
	})
}

// SubjectBudgetPolicies returns ONLY the subject-owned budget-scope policies
// (#473) — platform-owned caps are deliberately invisible to the subject.
func (s *Service) SubjectBudgetPolicies(ctx context.Context, payer identity.CustomerID) ([]BudgetScopePolicyInput, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	rows, err := admission.NewBudgetPolicyStore(s.rt.DB).LoadByOwner(ctx, payer, "subject")
	if err != nil {
		return nil, err
	}
	out := make([]BudgetScopePolicyInput, 0, len(rows))
	for _, r := range rows {
		w := make([]BudgetScopeWindowInput, 0, len(r.Windows))
		for _, ww := range r.Windows {
			w = append(w, BudgetScopeWindowInput{Key: ww.Key, WindowSeconds: ww.WindowSeconds, Limit: ww.Limit, Currency: ww.Currency, Cadence: ww.Cadence})
		}
		out = append(out, BudgetScopePolicyInput{Scope: r.Scope, ScopeKey: r.ScopeKey, Windows: w})
	}
	return out, nil
}

// TierBudgetWindowInput / TierPolicyInput configure a tier's money policy via
// the admin endpoint (#298: tier admin API).
type TierBudgetWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
	// Cadence is "session" (default) or "fixed" (#337 fixed windows).
	Cadence string `json:"cadence,omitempty"`
}

type TierPolicyInput struct {
	Tier           string                  `json:"tier"`
	BudgetWindows  []TierBudgetWindowInput `json:"budget_windows"`
	PolicyCurrency string                  `json:"policy_currency,omitempty"`
	// BadSpendWindows are the #497 per-PAYER direct-credential wasted-spend grace
	// windows for this tier: at most Limit of host-reported wasted spend is
	// forgiven per window; direct-payer overage is charged.
	BadSpendWindows []TierBudgetWindowInput `json:"bad_spend_windows,omitempty"`
}

// DefaultInvokerWastedWindows is the flat delegated-invoker wasted-spend default:
// invokers aren't trusted (an account mints unlimited invokers), so the
// per-invoker budget is a fixed backstop rather than tier-graduated. Amounts use
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
	DelegatedInvokerWastedSpendWindows []abuse.WastedWindow
}

// SetMerchantConfiguration persists the merchant-scoped configuration row. An
// empty DelegatedInvokerWastedSpendWindows slice clears that key to the
// DefaultInvokerWastedWindows fallback.
func (s *Service) SetMerchantConfiguration(ctx context.Context, in MerchantConfiguration) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	cfg := models.MerchantConfiguration{
		DelegatedInvokerWastedSpendWindows: make([]models.BudgetWindowPolicy, 0, len(in.DelegatedInvokerWastedSpendWindows)),
	}
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
// tier policy's bad_spend windows (#488) at the payer's current tier.
func (s *Service) payerWastedWindows(ctx context.Context, payer identity.CustomerID, currency, tier string) ([]abuse.WastedWindow, error) {
	if tier == "" {
		if t, err := s.GetTier(ctx, payer, currency); err == nil && t != "" {
			tier = t
		} else {
			tier = admission.DefaultTier
		}
	}
	pol, err := admission.NewTierPolicyStore(s.rt.DB).GetTierPolicy(ctx, payer, tier)
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
// money. Source+SourceID are required for retry idempotency.
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
// against tier-graduated payer grace and charge overage through the normal usage
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
	if err := money.ValidateCurrency(cur); err != nil {
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
			_, err = s.moneyService().RecordUsage(ctx, money.RecordUsageParams{
				Payer:     &in.CustomerID,
				Invoker:   strings.TrimSpace(in.Invoker),
				Currency:  cur,
				EventType: "wasted_spend",
				Amount:    chargeable,
				Source:    in.Source,
				SourceID:  in.SourceID,
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
	if err := money.ValidateCurrency(cur); err != nil {
		return "", err
	}
	for _, w := range windows {
		if strings.TrimSpace(w.Currency) == "" {
			continue
		}
		wc := money.NormalizeCurrency(w.Currency)
		if err := money.ValidateCurrency(wc); err != nil {
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

// AbuseUsageWindow is one wasted-spend window's running $ total for /abuse-usage.
type AbuseUsageWindow struct {
	Key        string `json:"key"`
	Currency   string `json:"currency"`
	Window     string `json:"window"`
	Used       int64  `json:"used"`
	Limit      int64  `json:"limit"`
	OverBudget bool   `json:"over_budget"`
}

// AbuseUsage returns the payer's and invoker's running wasted-$ totals + budgets
// (#488 introspection) for a host's /abuse-usage view — wasted $, not just
// counts. invoker "" omits the invoker windows.
func (s *Service) AbuseUsage(ctx context.Context, payer identity.CustomerID, invoker, currency, tier string) (payerWindows, invokerWindows []AbuseUsageWindow, err error) {
	if s == nil || s.rt == nil {
		return nil, nil, fmt.Errorf("service not initialized")
	}
	if s.rt.RedisClient == nil {
		return nil, nil, fmt.Errorf("wasted-spend tracking unavailable: redis not configured")
	}
	if payer.IsZero() {
		return nil, nil, fmt.Errorf("payer required")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return nil, nil, err
	}
	if err := money.ValidateCurrency(cur); err != nil {
		return nil, nil, err
	}
	tid, terr := merchant.Require(ctx)
	if terr != nil {
		return nil, nil, terr
	}
	merchant := tid.UUID().String()
	guard := abuse.NewWastedSpendGuard(ratelimit.NewLimiter(s.rt.RedisClient))

	pw, err := s.payerWastedWindows(ctx, payer, cur, tier)
	if err != nil {
		return nil, nil, err
	}
	payerPolicyCurrency, err := serviceWastedCurrency(cur, pw)
	if err != nil {
		return nil, nil, err
	}
	pUsage, err := guard.Usage(ctx, guard.PayerBase(merchant, payer.UUID().String(), payerPolicyCurrency), payerPolicyCurrency, pw)
	if err != nil {
		return nil, nil, err
	}
	payerWindows = toAbuseUsageWindows(pUsage)

	if invoker != "" {
		aw, err := s.invokerWastedSpendPolicy(ctx)
		if err != nil {
			return nil, nil, err
		}
		invokerPolicyCurrency, err := serviceWastedCurrency(cur, aw)
		if err != nil {
			return nil, nil, err
		}
		aUsage, err := guard.Usage(ctx, guard.InvokerBase(merchant, payer.UUID().String(), invoker, invokerPolicyCurrency), invokerPolicyCurrency, aw)
		if err != nil {
			return nil, nil, err
		}
		invokerWindows = toAbuseUsageWindows(aUsage)
	}
	return payerWindows, invokerWindows, nil
}

func toAbuseUsageWindows(ws []abuse.WindowUsage) []AbuseUsageWindow {
	out := make([]AbuseUsageWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, AbuseUsageWindow{
			Key: w.Key, Currency: w.Currency, Window: w.Window, Used: w.Used,
			Limit: w.Limit, OverBudget: w.OverBudget,
		})
	}
	return out
}

// SetTierPolicy upserts a per-payer tier money policy.
func (s *Service) SetTierPolicy(ctx context.Context, payer identity.CustomerID, in TierPolicyInput) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	// A ZERO payer writes the TENANT-WIDE DEFAULT tier policy (#477): the platform
	// capacity ladder declared once, applied to every payer at the tier. A non-zero
	// payer writes a per-subject override.
	if in.Tier == "" {
		return fmt.Errorf("tier required")
	}
	pol := models.ThroughputPolicy{PolicyCurrency: in.PolicyCurrency}
	for _, b := range in.BudgetWindows {
		pol.BudgetWindows = append(pol.BudgetWindows, models.BudgetWindowPolicy{Key: b.Key, WindowSeconds: b.WindowSeconds, Limit: b.Limit, Currency: b.Currency, Cadence: b.Cadence})
	}
	for _, b := range in.BadSpendWindows {
		pol.BadSpendWindows = append(pol.BadSpendWindows, models.BudgetWindowPolicy{Key: b.Key, WindowSeconds: b.WindowSeconds, Limit: b.Limit, Currency: b.Currency, Cadence: b.Cadence})
	}
	return admission.NewTierPolicyStore(s.rt.DB).UpsertTierPolicyFull(ctx, payer, in.Tier, pol)
}

// TierScheduleRung is one rung of a persisted same-currency tier ladder (#476):
// a payer reaches Tier once its cumulative paid spend in the schedule currency
// is at least MinCumulativePaidAmount.
type TierScheduleRung struct {
	Tier                    string `json:"tier"`
	MinCumulativePaidAmount int64  `json:"min_cumulative_paid_amount"`
}

// SetTierSchedule persists the merchant's tier SCHEDULE (#476): the host declares
// the same-currency ladder ONCE and OpenRails then AUTO-maintains each payer's
// tier from cumulative spend (no host cranking). A zero payer sets the
// merchant-wide default schedule; a non-zero payer sets a per-subject override.
// owner=platform.
func (s *Service) SetTierSchedule(ctx context.Context, payer identity.CustomerID, currency string, schedule []TierScheduleRung) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return err
	}
	if err := money.ValidateCurrency(cur); err != nil {
		return err
	}
	moneySvc := money.NewMoneyService(s.rt.DB)
	rungs := make([]money.TierThreshold, 0, len(schedule))
	for _, r := range schedule {
		rungs = append(rungs, money.TierThreshold{Tier: r.Tier, MinPaidAmount: r.MinCumulativePaidAmount})
	}
	return moneySvc.SetTierSchedule(ctx, payer, cur, rungs)
}

// GetTier returns the payer's CURRENT graduated tier (#477) for one currency:
// the tier OpenRails auto-maintains from same-currency cumulative paid spend
// against the persisted tier schedule (#476), or a manual admin override. Empty
// when the payer has never graduated (no tier set) — the caller treats that as
// the lowest/default tier.
func (s *Service) GetTier(ctx context.Context, payer identity.CustomerID, currency string) (string, error) {
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
	if err := money.ValidateCurrency(cur); err != nil {
		return "", err
	}
	return money.NewMoneyService(s.rt.DB).GetTier(ctx, payer, cur)
}
