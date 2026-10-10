package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/subscriptions"

	"github.com/google/uuid"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// AdmitInput is the host's admission request for payer capacity and delegated
// spend gates.
type AdmitInput struct {
	CustomerID  identity.CustomerID
	Invoker     string
	InvokerType billing.InvokerType
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
	// ExpiresAt is the deadline of the job this admit covers — REQUIRED when
	// EstimatedAmount places a hold; the hold lives exactly that long unless
	// captured, released or extended. Zero declares none.
	ExpiresAt time.Time
}

// ErrHoldDeadlineRequired is returned by Admit when EstimatedAmount places a
// hold and no ExpiresAt was declared.
var ErrHoldDeadlineRequired = admission.ErrHoldDeadlineRequired

// ErrHoldDeadlinePassed is returned by Admit/ExtendHold when the declared
// deadline is already in the past.
var ErrHoldDeadlinePassed = admission.ErrHoldDeadlinePassed

// ErrHoldNotFound is returned by ExtendHold when no live hold exists for the
// request id: it was captured, released, or lapsed at its declared deadline.
// The caller must re-admit; a lapsed hold is never resurrected.
var ErrHoldNotFound = errors.New("hold not found for request_id")

// Admit evaluates policy and reserves a durable request operation in one payer transaction.
func (s *Service) Admit(ctx context.Context, in AdmitInput) (*billing.Admission, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if in.CustomerID.IsZero() {
		return nil, &spendgate.ValidationError{Param: "customer_id", Message: "customer_id required"}
	}
	{
		in.SourceID = strings.TrimSpace(in.SourceID)
		if err := spendgate.ValidateRequest(in.SourceID, in.EstimatedAmount, in.AccrualRateDeltaPerHour); err != nil {
			return nil, err
		}
		if in.InvokerType != billing.InvokerTypeCustomer && in.InvokerType != billing.InvokerTypeDelegated {
			return nil, &spendgate.ValidationError{Param: "invoker_type", Message: "invoker_type must be customer or delegated"}
		}
	}
	currency, err := requireCurrency(in.Currency)
	if err != nil {
		return nil, err
	}

	gate := s.spendGate()
	loader := admission.NewSpendgatePolicyLoader(
		admission.NewBillingPolicyStore(s.rt.DB),
		admission.NewInvokerSpendLimitStore(s.rt.DB),
		s.rt.FXProvider,
	)
	adm := admission.NewAdmitter(s.moneyService(), gate, loader).
		WithFailedUsageCutoff(failedUsageCutoff{s}).
		WithDenialRecorder(admission.NewDenialRecorder(s.rt.RedisClient)).
		WithDelinquency(s.delinquencyService()).
		WithAccrualRateMeter(admission.NewAccrualRateMeter(s.rt.DB))

	// The hold's lifetime is the caller's declared job deadline, never a
	// default of ours (xs-007 row 33): the Admitter refuses a hold that
	// declares none (ErrHoldDeadlineRequired). A job that outlives its
	// estimate re-declares through ExtendHold.
	exp := in.ExpiresAt.UTC()
	source := in.Source
	if source == "" {
		source = "admit"
	}

	dec, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID:      in.CustomerID,
		Invoker:         in.Invoker,
		InvokerType:     string(in.InvokerType),
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

	res := &billing.Admission{
		RequestID:           in.SourceID,
		CustomerID:          billing.CustomerID(in.CustomerID),
		Allowed:             dec.Allowed,
		Currency:            currency,
		EstimatedAmount:     in.EstimatedAmount,
		StartCapacityAmount: startCapacity(dec.AvailableAmount, dec.HeldAmount),
		Replayed:            dec.Replayed,
	}
	if dec.Allowed {
		state := billing.AdmissionState(dec.State)
		res.State, res.ExpiresAt = &state, dec.HoldExpiresAt
	} else {
		blocked, code := billing.AdmissionBlock(dec.BlockedBy), dec.DenyCode
		res.BlockedBy, res.DenyCode = &blocked, &code
		if dec.RetryAfterSeconds > 0 {
			retry := dec.RetryAfterSeconds
			res.RetryAfterSeconds = &retry
		}
	}
	return res, nil
}

// admissionFromOperation is the stored state of an allowed admission.
func admissionFromOperation(row gen.BillingAdmissionOperation, now time.Time) *billing.Admission {
	state := billing.AdmissionState(row.State)
	if state == billing.AdmissionOpen && row.ExpiresAt != nil && !row.ExpiresAt.After(now) {
		state = billing.AdmissionExpired
	}
	return &billing.Admission{
		RequestID: row.RequestID, CustomerID: billing.CustomerID(row.CustomerID), Allowed: true,
		Currency: row.Currency, EstimatedAmount: row.EstimatedAmount, StartCapacityAmount: row.AvailableAmount,
		State: &state, ExpiresAt: row.ExpiresAt, CapturedAmount: row.CapturedAmount,
	}
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

func budgetScopeWindowModels(ws []billing.BudgetWindow) []models.BudgetWindowPolicy {
	out := make([]models.BudgetWindowPolicy, 0, len(ws))
	for _, w := range ws {
		out = append(out, models.BudgetWindowPolicy{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency})
	}
	return out
}

func spendLimitWindowInputs(ws []models.BudgetWindowPolicy) []billing.BudgetWindow {
	out := make([]billing.BudgetWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, billing.BudgetWindow{
			Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency,
		})
	}
	return out
}

func invokerSpendLimitKey(scope, scopeKey string) string {
	return budgets.NormalizeScope(scope) + "\x00" + strings.TrimSpace(scopeKey)
}

// ValidateSpendDelegations validates and canonicalizes a complete
// payer-owned spend-delegation document. Duplicate detection happens after
// scope and scope_key are normalized, so every transport has identical
// replacement semantics. or#893 deleted the role_id alias: a role delegation is
// {scope:"role", scope_key:"<role uuid>"} and nothing else.
func ValidateSpendDelegations(in []billing.SpendDelegation) ([]billing.SpendDelegation, error) {
	out := make([]billing.SpendDelegation, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, item := range in {
		scope := budgets.NormalizeScope(string(item.Scope))
		scopeKey := strings.TrimSpace(item.ScopeKey)
		row, err := admission.ValidateInvokerSpendLimit(admission.InvokerSpendLimit{
			Scope: scope, ScopeKey: scopeKey, Windows: budgetScopeWindowModels(item.Windows),
			Provenance: normalize.FromPtr(item.Provenance),
		})
		if err != nil {
			return nil, invalidInvokerSpendLimit(fmt.Sprintf("delegations[%d].%s", i, err))
		}
		key := invokerSpendLimitKey(row.Scope, row.ScopeKey)
		if _, duplicate := seen[key]; duplicate {
			return nil, invalidInvokerSpendLimit(fmt.Sprintf("duplicate delegation for %s", key))
		}
		seen[key] = struct{}{}
		out = append(out, billing.SpendDelegation{
			Scope: billing.SpendDelegationScope(row.Scope), ScopeKey: row.ScopeKey, Windows: spendLimitWindowInputs(row.Windows),
			Provenance: normalize.OptionalString(row.Provenance),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return invokerSpendLimitKey(string(out[i].Scope), out[i].ScopeKey) < invokerSpendLimitKey(string(out[j].Scope), out[j].ScopeKey)
	})
	return out, nil
}

func invokerSpendLimitRow(in billing.SpendDelegation) admission.InvokerSpendLimit {
	return admission.InvokerSpendLimit{
		Scope: string(in.Scope), ScopeKey: in.ScopeKey, Windows: budgetScopeWindowModels(in.Windows),
		Provenance: normalize.FromPtr(in.Provenance),
	}
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

// InvokerSpendWindows returns the spend windows a delegated invoker is enforced
// against on payer's account, with their live metering (or#930).
//
// It is a READ over the accounting admission already keeps: the same grants the
// admit path resolves (LoadDelegatedWindows), metered by the same durable SQL operations
// and hold records the gate writes. Nothing here counts anything.
//
// PAYER-SCOPE WINDOWS ARE DELIBERATELY ABSENT. The payer's own caps and product
// usage limits gate the whole account, not this invoker; showing them to one
// delegated user would report a budget it neither owns nor can act on, and would
// leak the account's aggregate posture. The payer reads those as the payer.
func (s *Service) InvokerSpendWindows(ctx context.Context, payer identity.CustomerID, in InvokerSpendWindowsInput) ([]billing.SpendWindow, error) {
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
	invoker := strings.TrimSpace(in.Invoker)
	if invoker == "" {
		return nil, fmt.Errorf("invoker required")
	}
	currency, err := requireCurrency(in.Currency)
	if err != nil {
		return nil, err
	}
	_, err = merchant.Require(ctx)
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
	)
	scopes, _, err := loader.LoadDelegatedWindows(ctx, payer, trustLevel, currency, req)
	if err != nil {
		return nil, err
	}

	gate := s.spendGate()
	usage, err := gate.WindowUsage(
		ctx, payer.UUID(), currency, spendgate.Policy{Scopes: scopes}, req)
	if err != nil {
		return nil, err
	}

	out := make([]billing.SpendWindow, 0, len(usage))
	for _, u := range usage {
		remaining := u.Limit - u.Used
		if remaining < 0 {
			remaining = 0
		}
		out = append(out, billing.SpendWindow{
			Scope:         spendWindowScope(u.Scope),
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

// spendWindowScope names a metered window by the delegation scope that
// declared it.
func spendWindowScope(scope spendgate.Scope) billing.SpendDelegationScope {
	if scope == spendgate.ScopeTrustLevel {
		return billing.SpendDelegationInvokerTier
	}
	return billing.SpendDelegationScope(scope)
}

// InvokerSpendLimits returns the payer's per-invoker spend limits (#473/#517).
func (s *Service) InvokerSpendLimits(ctx context.Context, payer identity.CustomerID) ([]billing.SpendDelegation, error) {
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
	out := make([]billing.SpendDelegation, 0, len(rows))
	for _, r := range rows {
		out = append(out, billing.SpendDelegation{Scope: billing.SpendDelegationScope(budgets.NormalizeScope(r.Scope)), ScopeKey: r.ScopeKey, Windows: spendLimitWindowInputs(r.Windows), Provenance: normalize.OptionalString(r.Provenance)})
	}
	sort.Slice(out, func(i, j int) bool {
		return invokerSpendLimitKey(string(out[i].Scope), out[i].ScopeKey) < invokerSpendLimitKey(string(out[j].Scope), out[j].ScopeKey)
	})
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
func (s *Service) ReplaceInvokerSpendLimits(ctx context.Context, payer identity.CustomerID, next []billing.SpendDelegation) error {
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
	normalized, err := ValidateSpendDelegations(next)
	if err != nil {
		return err
	}
	rows := make([]admission.InvokerSpendLimit, 0, len(normalized))
	for _, in := range normalized {
		rows = append(rows, invokerSpendLimitRow(in))
	}
	return admission.NewInvokerSpendLimitStore(s.rt.DB).Replace(ctx, payer, rows)
}

// BillingPolicy declares one named billing policy (or#897). Window entries
// carry the same {key, window_seconds, limit, currency} shape everywhere in this
// package — billing.BudgetWindow.
type BillingPolicy = billing.BillingPolicy

// BillingPolicyBinding points one rung at a policy name (or#897). Set
// CustomerID for the per-customer rung, Tier for the per-tier rung, neither for
// the merchant default — never both.
type BillingPolicyBinding struct {
	PolicyName string
	CustomerID billing.CustomerID
	Tier       string
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
	// subscriptions.DefaultPriceIncreaseNoticeDays when unset).
	RepriceNoticeWindowDays *int
	// RenewalReceiptMinIntervalHours (#1069) spaces renewal receipts per
	// subscription. A nil pointer preserves the stored value.
	RenewalReceiptMinIntervalHours *int
	// ProviderRefundAccess is the provider-dashboard refund access policy. A
	// nil pointer preserves the stored value.
	ProviderRefundAccess *string
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
	// DunningPolicy (#1093) replaces the dunning schedule. A nil pointer
	// preserves the stored policy.
	DunningPolicy *billing.DunningPolicy
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
		RenewalReceiptMinIntervalHours:     cfg.RenewalReceiptMinIntervalHours,
		ProviderRefundAccess:               nonEmptyString(cfg.ProviderRefundAccess),
		ArrearsGraceDays:                   cfg.ArrearsGraceDays,
		ArrearsDelinquencyFloor:            cfg.ArrearsDelinquencyFloor,
		CheckoutRouting:                    routing,
		DunningPolicy:                      cfg.DunningPolicy,
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
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		database := s.rt.DB.NewWithPgxTx(tx)
		if _, err := database.Gen(ctx).LockMerchantSettings(ctx, mid.UUID()); err != nil {
			return err
		}
		cfg, _, err := merchantconfig.NewStore(database).Get(ctx)
		if err != nil {
			return err
		}
		cfg, err = applyMerchantConfiguration(cfg, in)
		if err != nil {
			return err
		}
		return merchantconfig.NewStore(database).Upsert(ctx, cfg)
	})
}

func applyMerchantConfiguration(cfg models.MerchantConfiguration, in MerchantConfiguration) (models.MerchantConfiguration, error) {
	if in.Profile != nil {
		cfg.Profile = *in.Profile
	}
	if in.InvoiceCollectionThreshold != nil {
		if *in.InvoiceCollectionThreshold < 0 {
			return cfg, fmt.Errorf("collection_threshold must be >= 0")
		}
		cfg.InvoiceCollectionThreshold = in.InvoiceCollectionThreshold
	}
	if in.InvoiceMonthlyFloor != nil {
		if *in.InvoiceMonthlyFloor < 0 {
			return cfg, fmt.Errorf("monthly_floor must be >= 0")
		}
		cfg.InvoiceMonthlyFloor = in.InvoiceMonthlyFloor
	}
	if in.InvoiceBillingBoundary != "" {
		if money.NormalizeInvoiceBoundary(in.InvoiceBillingBoundary) == "" {
			return cfg, fmt.Errorf("invalid billing_period_boundary %q", in.InvoiceBillingBoundary)
		}
		cfg.InvoiceBillingBoundary = in.InvoiceBillingBoundary
	}
	if in.AlertEmail != nil {
		cfg.AlertEmail = strings.TrimSpace(*in.AlertEmail)
	}
	if in.RepriceNoticeWindowDays != nil {
		if *in.RepriceNoticeWindowDays < 0 {
			return cfg, fmt.Errorf("reprice_notice_window_days must be >= 0")
		}
		cfg.RepriceNoticeWindowDays = in.RepriceNoticeWindowDays
	}
	if in.RenewalReceiptMinIntervalHours != nil {
		if *in.RenewalReceiptMinIntervalHours < 0 {
			return cfg, fmt.Errorf("renewal_receipt_min_interval_hours must be >= 0")
		}
		cfg.RenewalReceiptMinIntervalHours = in.RenewalReceiptMinIntervalHours
	}
	if in.ProviderRefundAccess != nil {
		policy, err := merchantconfig.NormalizeProviderRefundAccess(strings.TrimSpace(*in.ProviderRefundAccess))
		if err != nil {
			return cfg, err
		}
		cfg.ProviderRefundAccess = policy
	}
	if in.ArrearsGraceDays != nil {
		if *in.ArrearsGraceDays < 0 {
			return cfg, fmt.Errorf("arrears_grace_days must be >= 0")
		}
		cfg.ArrearsGraceDays = in.ArrearsGraceDays
	}
	if in.ArrearsDelinquencyFloor != nil {
		if *in.ArrearsDelinquencyFloor < 0 {
			return cfg, fmt.Errorf("arrears_delinquency_floor must be >= 0")
		}
		cfg.ArrearsDelinquencyFloor = in.ArrearsDelinquencyFloor
	}
	if in.DunningPolicy != nil {
		if _, err := subscriptions.PolicyOf(in.DunningPolicy); err != nil {
			return cfg, err
		}
		cfg.DunningPolicy = in.DunningPolicy
	}
	if in.CheckoutRouting != nil {
		routing, err := merchantconfig.NormalizeCheckoutRouting(*in.CheckoutRouting)
		if err != nil {
			return cfg, err
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
	return cfg, nil
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

// failedUsageWindows resolves the windows a failed usage event counts toward,
// in currency: a delegated invoker's cutoff (the merchant's configuration, or
// DefaultInvokerWastedWindows), else the customer's grace (its billing policy's
// bad_spend_windows at its trust level). Limits in another currency are
// converted, as spend windows are.
func (s *Service) failedUsageWindows(ctx context.Context, customer identity.CustomerID, invokerType billing.InvokerType, currency string) ([]money.FailedUsageWindow, error) {
	var windows []abuse.WastedWindow
	var err error
	if invokerType == billing.InvokerTypeDelegated {
		windows, err = s.invokerWastedSpendPolicy(ctx)
	} else {
		windows, err = s.payerWastedWindows(ctx, customer, currency, "")
	}
	if err != nil {
		return nil, err
	}
	out := make([]money.FailedUsageWindow, 0, len(windows))
	for _, w := range windows {
		if w.Limit <= 0 || w.Window < time.Second {
			continue
		}
		limit := w.Limit
		if wc := money.NormalizeCurrency(w.Currency); wc != "" && wc != currency {
			if limit, _, err = fx.ConvertAmount(ctx, s.rt.FXProvider, wc, currency, w.Limit); err != nil {
				return nil, err
			}
		}
		out = append(out, money.FailedUsageWindow{Key: w.Key, Duration: w.Window, Limit: limit})
	}
	return out, nil
}

// failedUsageCutoff is admission's view of delegated invokers' failed usage.
type failedUsageCutoff struct{ s *Service }

func (c failedUsageCutoff) CutoffReached(ctx context.Context, customer identity.CustomerID, invoker, currency string) (bool, error) {
	windows, err := c.s.failedUsageWindows(ctx, customer, billing.InvokerTypeDelegated, currency)
	if err != nil || len(windows) == 0 {
		return false, err
	}
	reached, _, err := c.s.moneyService().FailedUsageCutoffReached(ctx, customer, invoker, currency, windows)
	return reached, err
}

// ErrInvalidBillingPolicy identifies caller-owned billing-policy input errors so
// HTTP and embedded transports map them to the same 400/ErrInvalid contract.
var ErrInvalidBillingPolicy = errors.New("invalid billing policy")

// ValidateBillingPolicy runs the ONE shared normalizer (or#288 pattern) over a
// declared policy. Both the manifest loader and this API call it, so a policy
// that boots cannot be one the API would have refused.
func ValidateBillingPolicy(in BillingPolicy) (string, models.BillingPolicy, error) {
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
func (s *Service) SetBillingPolicy(ctx context.Context, in BillingPolicy) error {
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
	return nil
}

// BindBillingPolicy points one rung (customer / tier / merchant default) at a
// declared policy name. This is the merchant's runtime lever: rebinding changes
// which cap applies to a payer and moves no money.
func (s *Service) BindBillingPolicy(ctx context.Context, in BillingPolicyBinding) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	customerRaw := in.CustomerID.String()
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
	return nil
}

// ListBillingPolicies returns every declared policy for the config-sync document.
func (s *Service) ListBillingPolicies(ctx context.Context) ([]BillingPolicy, error) {
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
	out := make([]BillingPolicy, 0, len(names))
	for _, name := range names {
		body := stored[name]
		out = append(out, BillingPolicy{
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
func (s *Service) ListBillingPolicyBindings(ctx context.Context) ([]billing.BillingPolicyBinding, error) {
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
	out := make([]billing.BillingPolicyBinding, 0, len(rows))
	for _, r := range rows {
		b := billing.BillingPolicyBinding{PolicyName: r.PolicyName, Tier: r.Tier}
		out = append(out, b)
	}
	return out, nil
}

// GetTrustLevel returns the payer's host-assigned trust level for one currency:
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

func nonEmptyString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
