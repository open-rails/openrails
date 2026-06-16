package admission

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Admitter is the service-admit money gate. It resolves the payer tier for money
// policy, reserves delegated spend windows, enforces delegated wasted-spend
// cutoffs, then asks the ledger whether the payer has enough prepaid/credit
// capacity for the request estimate.
// DefaultTier is assigned to actors with no explicit tier — the new-account low
// default (#300): start at the lowest tier until graduated.
const DefaultTier = "free"

// Wasted-spend deny codes (#488/#497). Payer bad_spend is no longer an admit
// cutoff; direct-payer overage is charged at report time. The remaining
// admit-time wasted-spend deny is the delegated invoker flat cutoff.
const DenyFailureRateLimited = "failure_rate_limited"

const DenyDelegatedSpendNotAllowed = "delegated_spend_not_allowed"

type Admitter struct {
	money        *money.MoneyService
	policies     *TierPolicyStore
	budgets      *budgets.Service   // optional; nil disables fixed money-budget windows (#304, #337)
	budgetScopes *BudgetPolicyStore // optional; nil disables hierarchical budget scopes (#473)
	fxProvider   fx.Provider        // optional; required for cross-currency policy enforcement

	// wasted is the optional $-valued wasted-spend guard (#488); nil disables the
	// wasted-spend admit gate. invokerWastedWindows is the FLAT per-invoker budget
	// default (invokers aren't trusted, so their budget isn't tier-graduated).
	wasted               *abuse.WastedSpendGuard
	invokerWastedWindows []abuse.WastedWindow
}

func NewAdmitter(moneySvc *money.MoneyService, policies *TierPolicyStore, budgetSvc *budgets.Service) *Admitter {
	return &Admitter{money: moneySvc, policies: policies, budgets: budgetSvc}
}

// WithBudgetScopes enables hierarchical budget-scope composition (#473): the
// platform (subject) cap, the subject's self (subject) cap, every (subject,role)
// cap the invoker holds, plus the (subject,invoker) cap — all reserved in one
// verdict. Nil store leaves only the pre-#473 (subject,invoker) tier-policy path.
func (a *Admitter) WithBudgetScopes(store *BudgetPolicyStore) *Admitter {
	a.budgetScopes = store
	return a
}

func (a *Admitter) WithFXProvider(provider fx.Provider) *Admitter {
	a.fxProvider = provider
	return a
}

// WithWastedSpend enables the delegated-invoker wasted-spend admit gate (#497):
// deny delegated invokers when they are over the flat per-invoker wasted budget.
// Direct payer credentials are not cut off here; their wasted-spend reports use
// payer grace then normal ledger charging.
func (a *Admitter) WithWastedSpend(guard *abuse.WastedSpendGuard, invokerWindows []abuse.WastedWindow) *Admitter {
	a.wasted = guard
	a.invokerWastedWindows = invokerWindows
	return a
}

// AdmitRequest is one admission decision input.
type AdmitRequest struct {
	CustomerID  identity.CustomerID // the merchant subject
	Invoker     string              // canonical invoker: user:<id> / serviceToken:<key_id> / <issuer>:<sub>
	InvokerType string              // "payer" for direct payer credential; empty/other = delegated
	Tier        string              // optional payer trust tier; empty resolves from OpenRails money state
	Resource    string              // caller-supplied resource string for host-side attribution only

	// Roles are the immutable role UUIDs the invoker holds (#473). The host reads
	// them from the delegated JWT/permission set. Each role with a matching
	// (subject, role) budget policy gates this request's spend.
	Roles []uuid.UUID

	// Money axis (skipped when EstimatedAmount == 0). Amount precision is implied by Currency.
	Currency        string
	EstimatedAmount int64
	Source          string    // idempotency namespace (e.g. "usage")
	SourceID        string    // idempotency id (e.g. request id)
	ExpiresAt       time.Time // hold expiry
}

// AdmitDecision is the unified outcome.
type AdmitDecision struct {
	Allowed    bool
	BlockedBy  string // "budget" | "abuse" | "money" | ""
	DenyCode   string // money: the ledger deny code
	RetryAfter time.Duration
	// AccountCapacityAmount is prepaid availability plus remaining credit
	// capacity before active Redis holds for this request are placed.
	AccountCapacityAmount int64

	// BudgetReservationID is the (subject,invoker)-scope money-budget reservation
	// placed when allowed (#304, kept for back-compat); BudgetWindows is the
	// flattened per-window state for introspection.
	BudgetReservationID uuid.UUID
	BudgetWindows       []budgets.WindowStatus

	// BudgetScopes is the per-scope state for the composed multi-scope verdict
	// (#473): platform (subject) + subject (subject) + every (subject,role) +
	// (subject,invoker). On a budget deny, the blocking scope is the one whose
	// windows contain the breached (Allowed=false) window.
	BudgetScopes []budgets.ScopeStatus
	// BudgetSource/BudgetSourceID are the idempotency coords the reservations
	// were placed under, so settlement can capture/release ALL scopes by coords.
	BudgetSource   string
	BudgetSourceID string

	PolicyCurrency string
	PolicyAmount   int64
}

// Admit reserves delegated spend windows, checks delegated wasted-spend, then
// asks the money ledger for request capacity.
func (a *Admitter) Admit(ctx context.Context, req AdmitRequest) (AdmitDecision, error) {
	// Tier resolution: explicit tier > graduated tier (#298, earned from paid
	// spend) > lowest default (#300 new-account low default). The tier now loads
	// only money policy: delegated budgets, policy currency, and wasted-spend
	// windows. It is not endpoint/resource authorization.
	tier := req.Tier
	if tier == "" {
		if req.EstimatedAmount > 0 {
			if t, terr := a.money.GetTier(ctx, req.CustomerID, req.Currency); terr == nil && t != "" {
				tier = t
			}
		}
		if tier == "" {
			tier = DefaultTier
		}
	}

	pol, err := a.policies.GetTierPolicy(ctx, req.CustomerID, tier)
	if err != nil {
		return AdmitDecision{}, err
	}

	policyCurrency, err := effectivePolicyCurrency(req.Currency, pol.PolicyCurrency, nil)
	if err != nil {
		return AdmitDecision{}, err
	}
	policyAmount := req.EstimatedAmount
	if req.EstimatedAmount > 0 {
		policyAmount, _, err = fx.ConvertAmount(ctx, a.fxProvider, req.Currency, policyCurrency, req.EstimatedAmount)
		if err != nil {
			return AdmitDecision{}, err
		}
	}
	tid, err := merchant.Require(ctx) // #336: no default merchant
	if err != nil {
		return AdmitDecision{}, err
	}
	tenantID := tid.UUID()

	// --- wasted-spend $ budgets (#497): delegated invokers are cut off when
	// their flat per-invoker wasted budget is over. Direct payer credentials are
	// not denied here; ReportWastedSpend charges over-grace waste through the
	// normal ledger. ---
	if a.wasted != nil && a.wasted.Enabled() {
		if !identity.IsDirectPayerInvoker(req.InvokerType) && req.Invoker != "" && len(a.invokerWastedWindows) > 0 {
			wastedCurrency, werr := effectiveWastedCurrency(req.Currency, a.invokerWastedWindows)
			if werr != nil {
				return AdmitDecision{}, werr
			}
			over, _, werr := a.wasted.InvokerOverBudget(ctx, tenantID.String(), req.CustomerID.UUID().String(), req.Invoker, wastedCurrency, a.invokerWastedWindows)
			if werr != nil {
				return AdmitDecision{}, werr
			}
			if over {
				return AdmitDecision{Allowed: false, BlockedBy: "abuse", DenyCode: DenyFailureRateLimited, PolicyCurrency: wastedCurrency}, nil
			}
		}
	}

	// --- money-budget windows (#304/#337 fixed windows; #473 hierarchical
	// scopes): per-scope spend caps composed in one verdict over the ONE payer
	// balance. Each scope reserves a separate budget_reservations row (a GATE,
	// not a wallet); the single balance debit is placed by the ledger below. ---
	var budgetResID uuid.UUID
	var budgetWindows []budgets.WindowStatus
	var budgetScopeStatuses []budgets.ScopeStatus
	if req.EstimatedAmount > 0 && !identity.IsDirectPayerInvoker(req.InvokerType) && a.budgets == nil {
		return AdmitDecision{
			Allowed:        false,
			BlockedBy:      "budget",
			DenyCode:       DenyDelegatedSpendNotAllowed,
			PolicyCurrency: policyCurrency,
			PolicyAmount:   policyAmount,
		}, nil
	}
	if a.budgets != nil && req.EstimatedAmount > 0 {
		ttl := time.Hour
		if !req.ExpiresAt.IsZero() {
			if d := time.Until(req.ExpiresAt); d > 0 {
				ttl = d
			}
		}

		scopes, err := a.buildBudgetScopes(ctx, req)
		if err != nil {
			return AdmitDecision{}, err
		}
		if !identity.IsDirectPayerInvoker(req.InvokerType) && len(scopes) == 0 {
			return AdmitDecision{
				Allowed:        false,
				BlockedBy:      "budget",
				DenyCode:       DenyDelegatedSpendNotAllowed,
				PolicyCurrency: policyCurrency,
				PolicyAmount:   policyAmount,
			}, nil
		}
		if len(scopes) > 0 {
			policyCurrency, err = effectiveScopeCurrency(policyCurrency, scopes)
			if err != nil {
				return AdmitDecision{}, err
			}
			policyAmount, _, err = fx.ConvertAmount(ctx, a.fxProvider, req.Currency, policyCurrency, req.EstimatedAmount)
			if err != nil {
				return AdmitDecision{}, err
			}
			// Budgets and the ledger use the same currency internal precision. The
			// estimate reserves 1:1 against every matching scope's windows.
			statuses, ok, blockIdx, err := a.budgets.ReserveScopes(ctx, req.CustomerID, scopes, policyCurrency, policyAmount, req.Source, req.SourceID, ttl)
			if err != nil {
				return AdmitDecision{}, err
			}
			budgetScopeStatuses = statuses
			budgetWindows = flattenScopeWindows(statuses)
			for _, st := range statuses {
				if st.Scope == budgets.ScopeInvoker && st.Reserved != uuid.Nil {
					budgetResID = st.Reserved
				}
			}
			if !ok {
				return AdmitDecision{
					Allowed:        false,
					BlockedBy:      "budget",
					RetryAfter:     scopeRetry(statuses, blockIdx),
					BudgetWindows:  budgetWindows,
					BudgetScopes:   statuses,
					PolicyCurrency: policyCurrency,
					PolicyAmount:   policyAmount,
				}, nil
			}
		}
	}

	// --- money axis (reserve the estimate via the existing ledger gate) ---
	if req.EstimatedAmount > 0 {
		res, err := a.money.AuthorizeAndHold(ctx, money.AuthorizeHoldInput{
			Payer:           req.CustomerID,
			Invoker:         req.Invoker,
			Currency:        req.Currency,
			EstimatedAmount: req.EstimatedAmount,
			Source:          req.Source,
			SourceID:        req.SourceID,
			ExpiresAt:       req.ExpiresAt,
		})
		if err != nil {
			return AdmitDecision{}, err
		}
		if !res.Decision.Allowed {
			// Roll back ALL reserved budget scopes so a money-denied request
			// doesn't consume any of the payer's budget windows.
			if len(budgetScopeStatuses) > 0 && a.budgets != nil {
				_ = a.budgets.ReleaseByCoords(ctx, req.CustomerID, policyCurrency, req.Source, req.SourceID)
			}
			return AdmitDecision{
				Allowed:               false,
				BlockedBy:             "money",
				DenyCode:              res.Decision.DenyCode,
				AccountCapacityAmount: res.AccountCapacityAmount,
				BudgetWindows:         budgetWindows,
				BudgetScopes:          budgetScopeStatuses,
				PolicyCurrency:        policyCurrency,
				PolicyAmount:          policyAmount,
			}, nil
		}
		return AdmitDecision{Allowed: true, AccountCapacityAmount: res.AccountCapacityAmount, BudgetReservationID: budgetResID, BudgetWindows: budgetWindows, BudgetScopes: budgetScopeStatuses, BudgetSource: req.Source, BudgetSourceID: req.SourceID, PolicyCurrency: policyCurrency, PolicyAmount: policyAmount}, nil
	}

	return AdmitDecision{Allowed: true, BudgetReservationID: budgetResID, BudgetWindows: budgetWindows, BudgetScopes: budgetScopeStatuses, BudgetSource: req.Source, BudgetSourceID: req.SourceID, PolicyCurrency: policyCurrency, PolicyAmount: policyAmount}, nil
}

// buildBudgetScopes assembles customer-funded delegated-spend policy for this
// request. A matching invoker, invoker-tier, or role scope in budget_policies is
// an explicit grant that this invoker may spend the payer's money. Subject-wide
// caps are not treated as a delegated-spend grant.
func (a *Admitter) buildBudgetScopes(ctx context.Context, req AdmitRequest) ([]budgets.ScopeReservation, error) {
	var scopes []budgets.ScopeReservation

	if a.budgetScopes == nil {
		return scopes, nil
	}

	policies, err := a.budgetScopes.LoadAll(ctx, req.CustomerID)
	if err != nil {
		return nil, err
	}
	roles := make(map[string]uuid.UUID, len(req.Roles))
	for _, roleID := range req.Roles {
		if roleID != uuid.Nil {
			roles[roleID.String()] = roleID
		}
	}
	for _, p := range policies {
		windows := toBudgetWindows(p.Windows)
		if len(windows) == 0 {
			continue
		}
		switch budgets.NormalizeScope(p.Scope) {
		case budgets.ScopeInvoker:
			if p.ScopeKey != req.Invoker {
				continue
			}
			sc, err := budgets.MakeScopeReservation(budgets.ScopeInvoker, p.Owner, req.Invoker, uuid.Nil, windows)
			if err != nil {
				return nil, err
			}
			scopes = append(scopes, sc)
		case budgets.ScopeRole:
			roleID, ok := roles[p.ScopeKey]
			if !ok {
				continue
			}
			sc, err := budgets.MakeScopeReservation(budgets.ScopeRole, p.Owner, req.Invoker, roleID, windows)
			if err != nil {
				return nil, err
			}
			scopes = append(scopes, sc)
		case budgets.ScopeInvokerTier:
			if strings.TrimSpace(p.ScopeKey) == "" || p.ScopeKey != strings.TrimSpace(req.Tier) {
				continue
			}
			sc, err := budgets.MakeInvokerTierScopeReservation(p.Owner, p.ScopeKey, req.Invoker, windows)
			if err != nil {
				return nil, err
			}
			scopes = append(scopes, sc)
		}
	}
	return scopes, nil
}

// flattenScopeWindows concatenates every scope's window statuses for the
// introspection field (back-compat with the single-scope BudgetWindows shape).
func flattenScopeWindows(scopes []budgets.ScopeStatus) []budgets.WindowStatus {
	var out []budgets.WindowStatus
	for _, sc := range scopes {
		out = append(out, sc.Windows...)
	}
	return out
}

// scopeRetry returns the soonest reset of the blocking scope's breached windows.
func scopeRetry(scopes []budgets.ScopeStatus, blockIdx int) time.Duration {
	if blockIdx >= 0 && blockIdx < len(scopes) {
		return budgetRetry(scopes[blockIdx].Windows)
	}
	// Fallback: scan all scopes for the soonest breached reset.
	var best time.Duration
	for _, sc := range scopes {
		if d := budgetRetry(sc.Windows); d > 0 && (best == 0 || d < best) {
			best = d
		}
	}
	return best
}

func budgetRetry(ws []budgets.WindowStatus) time.Duration {
	var best int64
	for _, w := range ws {
		if !w.Allowed && w.RetryAfterSeconds > 0 && (best == 0 || w.RetryAfterSeconds < best) {
			best = w.RetryAfterSeconds
		}
	}
	return time.Duration(best) * time.Second
}

// toWastedWindows maps the tier policy's $-budget windows to the wasted-spend
// guard's window shape (#488). Window seconds → duration; the budget key is the
// window unit so each window is its own Redis counter.
func toWastedWindows(ws []models.BudgetWindowPolicy) []abuse.WastedWindow {
	out := make([]abuse.WastedWindow, 0, len(ws))
	for _, w := range ws {
		if w.WindowSeconds <= 0 {
			continue
		}
		out = append(out, abuse.WastedWindow{
			Key:      w.Key,
			Window:   time.Duration(w.WindowSeconds) * time.Second,
			Limit:    w.Limit,
			Currency: w.Currency,
		})
	}
	return out
}

func effectivePolicyCurrency(requestCurrency, configured string, windows []budgets.BudgetWindow) (string, error) {
	cur := money.NormalizeCurrency(requestCurrency)
	explicit := false
	if configured != "" {
		cur = money.NormalizeCurrency(configured)
		explicit = true
	}
	if err := money.ValidateCurrency(cur); err != nil {
		return "", err
	}
	for _, w := range windows {
		if w.Currency == "" {
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
			return "", fmt.Errorf("mixed policy currencies are not supported in one admit policy: %s and %s", cur, wc)
		}
	}
	return cur, nil
}

func effectiveScopeCurrency(requestCurrency string, scopes []budgets.ScopeReservation) (string, error) {
	cur := money.NormalizeCurrency(requestCurrency)
	explicit := false
	if err := money.ValidateCurrency(cur); err != nil {
		return "", err
	}
	for _, sc := range scopes {
		for _, w := range sc.Windows {
			if w.Currency == "" {
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
				return "", fmt.Errorf("mixed policy currencies are not supported in one admit policy: %s and %s", cur, wc)
			}
		}
	}
	return cur, nil
}

func effectiveWastedCurrency(requestCurrency string, windows []abuse.WastedWindow) (string, error) {
	cur := money.NormalizeCurrency(requestCurrency)
	explicit := false
	if err := money.ValidateCurrency(cur); err != nil {
		return "", err
	}
	for _, w := range windows {
		if w.Currency == "" {
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
		if wc != cur {
			return "", fmt.Errorf("mixed wasted-spend currencies are not supported in one policy: %s and %s", cur, wc)
		}
	}
	return cur, nil
}
