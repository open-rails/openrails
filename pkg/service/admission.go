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
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
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
	Tier        string // payer trust tier
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
	Allowed bool      `json:"allowed"`
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
		WithDenialRecorder(admission.NewDenialRecorder(s.rt.RedisClient))

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
		StartCapacityAmount: startCapacity(dec.AvailableAmount, dec.HeldAmount),
		BlockedBy:           dec.BlockedBy,
		DenyCode:            dec.DenyCode,
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
// policy (#473): same shape as TierBudgetWindowInput.
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
	Windows  []SpendLimitWindowInput `json:"windows"`
}

func budgetScopeWindowModels(ws []SpendLimitWindowInput) []models.BudgetWindowPolicy {
	out := make([]models.BudgetWindowPolicy, 0, len(ws))
	for _, w := range ws {
		out = append(out, models.BudgetWindowPolicy{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency})
	}
	return out
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
	return admission.NewInvokerSpendLimitStore(s.rt.DB).Upsert(ctx, payer, admission.InvokerSpendLimit{
		Scope: in.Scope, ScopeKey: in.ScopeKey,
		Windows: budgetScopeWindowModels(in.Windows),
	})
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
	store := admission.NewInvokerSpendLimitStore(s.rt.DB)
	existing, err := store.LoadAll(ctx, payer)
	if err != nil {
		return err
	}
	wanted := make(map[string]admission.InvokerSpendLimit, len(next))
	for _, in := range next {
		row := admission.InvokerSpendLimit{
			Scope:    in.Scope,
			ScopeKey: strings.TrimSpace(in.ScopeKey),
			Windows:  budgetScopeWindowModels(in.Windows),
		}
		wanted[row.Scope+"\x00"+row.ScopeKey] = row
	}
	for _, row := range existing {
		if _, keep := wanted[row.Scope+"\x00"+row.ScopeKey]; keep {
			continue
		}
		if err := store.Delete(ctx, payer, row.Scope, row.ScopeKey); err != nil {
			return err
		}
	}
	for _, row := range wanted {
		if err := store.Upsert(ctx, payer, row); err != nil {
			return err
		}
	}
	return nil
}

// TierBudgetWindowInput / PayerSpendLimitInput configure a trust tier's money
// policy via the admin endpoint (#298: tier admin API).
type TierBudgetWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	Limit         int64  `json:"limit"`
	Currency      string `json:"currency,omitempty"`
}

type PayerSpendLimitInput struct {
	TrustTier string `json:"trust_tier,omitempty"`
	// Tier is a deprecated alias for TrustTier, kept while current clients migrate.
	Tier           string                  `json:"tier,omitempty"`
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
	Profile                            *models.MerchantProfileConfiguration
	InvoiceCollectionThreshold         *int64
	InvoiceMonthlyFloor                *int64
	InvoiceBillingBoundary             string
	DelegatedInvokerWastedSpendWindows []abuse.WastedWindow
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
	out := MerchantConfiguration{
		Profile:                            &cfg.Profile,
		InvoiceCollectionThreshold:         cfg.InvoiceCollectionThreshold,
		InvoiceMonthlyFloor:                cfg.InvoiceMonthlyFloor,
		InvoiceBillingBoundary:             cfg.InvoiceBillingBoundary,
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
// trust-tier policy's bad_spend windows (#488) at the payer's current trust tier.
func (s *Service) payerWastedWindows(ctx context.Context, payer identity.CustomerID, currency, tier string) ([]abuse.WastedWindow, error) {
	if tier == "" {
		if t, err := s.GetTier(ctx, payer, currency); err == nil && t != "" {
			tier = t
		} else {
			tier = admission.DefaultTier
		}
	}
	pol, err := admission.NewPayerSpendLimitStore(s.rt.DB).GetPayerSpendLimits(ctx, payer, tier)
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

// SetPayerSpendLimits upserts a per-payer trust-tier money policy.
func (s *Service) SetPayerSpendLimits(ctx context.Context, payer identity.CustomerID, in PayerSpendLimitInput) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	// A ZERO payer writes the TENANT-WIDE DEFAULT trust-tier policy (#477): the
	// platform capacity ladder declared once, applied to every payer at the trust tier. A non-zero
	// payer writes a per-subject override.
	tier := strings.TrimSpace(in.TrustTier)
	if tier == "" {
		tier = strings.TrimSpace(in.Tier)
	}
	if tier == "" {
		return fmt.Errorf("trust_tier required")
	}
	pol := models.TierMoneyPolicy{PolicyCurrency: in.PolicyCurrency}
	for _, b := range in.BudgetWindows {
		pol.BudgetWindows = append(pol.BudgetWindows, models.BudgetWindowPolicy{Key: b.Key, WindowSeconds: b.WindowSeconds, Limit: b.Limit, Currency: b.Currency})
	}
	for _, b := range in.BadSpendWindows {
		pol.BadSpendWindows = append(pol.BadSpendWindows, models.BudgetWindowPolicy{Key: b.Key, WindowSeconds: b.WindowSeconds, Limit: b.Limit, Currency: b.Currency})
	}
	return admission.NewPayerSpendLimitStore(s.rt.DB).UpsertPayerSpendLimitsFull(ctx, payer, tier, pol)
}

// TierScheduleRung is one rung of a persisted same-currency tier ladder (#476):
// a payer reaches Tier once its cumulative paid spend in the schedule currency
// is at least MinCumulativePaidAmount.
type TierScheduleRung struct {
	Tier                    string `json:"tier"`
	MinCumulativePaidAmount int64  `json:"min_cumulative_paid_amount"`
}

// SetTierSchedule persists the merchant's trust-tier SCHEDULE (#476): the host declares
// the same-currency ladder ONCE and OpenRails then AUTO-maintains each payer's
// trust tier from cumulative spend (no host cranking). A zero payer sets the
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

// GetTier returns the payer's current trust tier (#477) for one currency:
// the value OpenRails auto-maintains from same-currency cumulative paid spend
// against the persisted trust-tier schedule (#476), or a manual admin override.
// Empty means the caller treats it as the lowest/default trust tier.
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
