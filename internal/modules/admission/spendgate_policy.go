package admission

import (
	"context"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SpendgatePolicyLoader builds a spendgate.Policy (the cap config the Redis admit
// gate enforces) for one (payer, trustLevel, request) from the or#897 billing
// policy registry + invoker_spend_limits. Every window's limit is expressed
// in the REQUEST currency (FX-converted when a window is denominated in another
// currency), so the gate runs in a single currency per admit. #513: this read
// replaces the per-request Postgres budget reservation; nothing on the hot path
// locks Postgres.
type SpendgatePolicyLoader struct {
	policies *BillingPolicyStore
	budgets  *InvokerSpendLimitStore
	fx       fx.Provider
	cache    *PolicyCache // optional; nil reads config from Postgres every load
}

// NewSpendgatePolicyLoader wires the loader. budgetScopes may be nil (then only
// the bound policy's payer-scope windows are loaded).
func NewSpendgatePolicyLoader(policies *BillingPolicyStore, budgetScopes *InvokerSpendLimitStore, fxp fx.Provider) *SpendgatePolicyLoader {
	return &SpendgatePolicyLoader{policies: policies, budgets: budgetScopes, fx: fxp}
}

// WithCache attaches the process-local policy-config cache (nil disables it; the
// loader then resolves the binding from Postgres on every load).
func (l *SpendgatePolicyLoader) WithCache(c *PolicyCache) *SpendgatePolicyLoader {
	l.cache = c
	return l
}

// ResolvePolicy returns the payer's bound billing policy, served from the cache
// when warm. Split out from Load so the admitter can decide the ARREARS
// question (does outstanding owed reduce headroom?) from the policy KIND before
// it reads capacity.
func (l *SpendgatePolicyLoader) ResolvePolicy(ctx context.Context, payer identity.CustomerID, trustLevel string) (ResolvedPolicy, error) {
	// Cache key needs the merchant so a payer uuid can't alias across merchants.
	tid, err := merchant.Require(ctx)
	if err != nil {
		return ResolvedPolicy{}, err
	}
	return l.cache.ResolvedPolicy(tid.UUID().String(), payer.UUID().String(), trustLevel, func() (ResolvedPolicy, error) {
		return l.policies.Resolve(ctx, payer, trustLevel)
	})
}

// Load turns an already-resolved policy plus the payer's delegated grants into
// the scoped windows the Redis gate enforces, in requestCurrency. req supplies
// the principals so the invoker_tier scope (whose policy key IS the trust level)
// resolves to a per-invoker window that applies only at the matching trust level.
//
// hasDelegatedGrant reports whether any invoker/role-scoped window matched the
// request — the caller uses it to deny a delegated invoker that has no explicit
// spend grant (the pre-existing "delegated_spend_not_allowed" guarantee).
func (l *SpendgatePolicyLoader) Load(ctx context.Context, payer identity.CustomerID, trustLevel, requestCurrency string, req spendgate.Request, resolved ResolvedPolicy) (pol spendgate.Policy, hasDelegatedGrant bool, err error) {
	var scopes []spendgate.ScopedWindows

	// The bound policy's NEW-spend windows (populated only by window_spend_cap;
	// an outstanding_cap policy caps debt, not velocity, and declares none).
	pw, err := l.convert(ctx, spendgate.ScopePayer, resolved.SpendWindows, requestCurrency)
	if err != nil {
		return spendgate.Policy{}, false, err
	}
	if len(pw) > 0 {
		scopes = append(scopes, spendgate.ScopedWindows{Scope: spendgate.ScopePayer, Windows: pw})
	}
	productLimits, err := l.policies.GetProductUsageLimitWindows(ctx, payer, req.Measure)
	if err != nil {
		return spendgate.Policy{}, false, err
	}
	plw, err := l.convert(ctx, spendgate.ScopePayer, productLimits, requestCurrency)
	if err != nil {
		return spendgate.Policy{}, false, err
	}
	if len(plw) > 0 {
		scopes = append(scopes, spendgate.ScopedWindows{Scope: spendgate.ScopePayer, Windows: plw})
	}

	if l.budgets != nil {
		// Invoker spend limits gate delegated GRANTS (hasDelegatedGrant) — a freshly
		// added grant must take effect immediately, so these are read LIVE, never
		// cached: a stale-absent grant would wrongly deny a just-granted invoker for the
		// whole cache TTL (#517). Only the per-trust-level payer CAPS above are cached
		// (benign when stale: a missing upper-bound briefly admits a little more, never denies).
		policies, lerr := l.budgets.LoadAll(ctx, payer)
		if lerr != nil {
			return spendgate.Policy{}, false, lerr
		}
		roleMatch := make(map[string]bool, len(req.Roles))
		for _, r := range req.Roles {
			roleMatch[r] = true
		}
		for _, p := range policies {
			bw := toBudgetWindows(p.Windows)
			switch budgets.NormalizeScope(p.Scope) {
			case budgets.ScopeInvoker:
				w, cerr := l.convert(ctx, spendgate.ScopeInvoker, bw, requestCurrency)
				if cerr != nil {
					return spendgate.Policy{}, false, cerr
				}
				if len(w) > 0 {
					scopes = append(scopes, spendgate.ScopedWindows{Scope: spendgate.ScopeInvoker, ScopeID: p.ScopeKey, Windows: w})
					if p.ScopeKey == req.Invoker {
						hasDelegatedGrant = true
					}
				}
			case budgets.ScopeRole:
				w, cerr := l.convert(ctx, spendgate.ScopeRole, bw, requestCurrency)
				if cerr != nil {
					return spendgate.Policy{}, false, cerr
				}
				if len(w) > 0 {
					scopes = append(scopes, spendgate.ScopedWindows{Scope: spendgate.ScopeRole, ScopeID: p.ScopeKey, Windows: w})
					if roleMatch[p.ScopeKey] {
						hasDelegatedGrant = true
					}
				}
			case budgets.ScopeInvokerTrustLevel:
				// Per-invoker cap selected by trust level: applies only at the matching level.
				if strings.TrimSpace(p.ScopeKey) != strings.TrimSpace(trustLevel) || strings.TrimSpace(req.Invoker) == "" {
					continue
				}
				w, cerr := l.convert(ctx, spendgate.ScopeInvoker, bw, requestCurrency)
				if cerr != nil {
					return spendgate.Policy{}, false, cerr
				}
				// Prefix the key so a trust-level-selected bucket can't alias a plain invoker window.
				for i := range w {
					w[i].Key = "it:" + trustLevel + ":" + w[i].Key
				}
				if len(w) > 0 {
					scopes = append(scopes, spendgate.ScopedWindows{Scope: spendgate.ScopeInvoker, ScopeID: req.Invoker, Windows: w})
					hasDelegatedGrant = true
				}
			}
		}
	}
	return spendgate.Policy{Scopes: scopes}, hasDelegatedGrant, nil
}

// convert maps budgets.BudgetWindow → spendgate.Window, FX-converting the limit to
// requestCurrency when the window carries a different currency.
func (l *SpendgatePolicyLoader) convert(ctx context.Context, scope spendgate.Scope, ws []budgets.BudgetWindow, requestCurrency string) ([]spendgate.Window, error) {
	reqCur := money.NormalizeCurrency(requestCurrency)
	out := make([]spendgate.Window, 0, len(ws))
	for _, w := range ws {
		if w.WindowSeconds <= 0 || w.Limit < 0 {
			continue
		}
		limit := w.Limit
		if c := strings.TrimSpace(w.Currency); c != "" && money.NormalizeCurrency(c) != reqCur {
			conv, _, err := fx.ConvertAmount(ctx, l.fx, money.NormalizeCurrency(c), reqCur, w.Limit)
			if err != nil {
				return nil, err
			}
			limit = conv
		}
		out = append(out, spendgate.Window{
			Scope:    scope,
			Duration: time.Duration(w.WindowSeconds) * time.Second,
			Limit:    limit,
			Key:      w.Key,
		})
	}
	return out, nil
}

// payerCapacity is the affordability snapshot the spendgate is evaluated
// against: spendable balance (ledger customer_balance counters, O(1)) plus the
// arrears credit line still available, used as the gate's negative floor.
// In-flight request holds are enforced by Redis.
//
// The bound policy's KIND decides whether prior debt reduces the line, and that
// single branch is the whole distinction between or#897's two seed businesses:
//
//   - outstanding_cap (and the no-binding default): the line is a ceiling on
//     DEBT, so outstanding owed is SUBTRACTED. $155 unpaid against a $200 line
//     leaves $45. Before or#878/or#897 the raw limit was passed and — because
//     arrears debt lives on the arrears account rather than as a negative
//     customer_balance — the cap never bit at all.
//   - window_spend_cap: the line is untouched by prior debt. The cloud business
//     caps NEW spend per window; an unpaid invoice feeds delinquency (the TIME
//     axis, or#878) and the merchant's own shutoff, not admission.
//
// The policy's declared OutstandingCapAmount wins when set; zero defers to the
// payer's own arrears credit limit, which stays the per-account lever.
func payerCapacity(ctx context.Context, moneySvc *money.MoneyService, payer identity.CustomerID, currency string, policy ResolvedPolicy) (available, creditLine, outstanding int64, err error) {
	capacity, err := moneySvc.GetAdmissionCapacity(ctx, payer, currency)
	if err != nil {
		return 0, 0, 0, err
	}
	available = capacity.Balance - capacity.Held
	outstanding = capacity.OutstandingOwed
	if capacity.BillingMode != money.BillingModeArrears {
		return available, 0, outstanding, nil
	}
	limit := capacity.CreditLimit
	if policy.OutstandingCapAmount > 0 {
		limit = policy.OutstandingCapAmount
	}
	if limit <= 0 {
		return available, 0, outstanding, nil
	}
	creditLine = limit
	if policy.GatesOnOutstandingOwed() {
		creditLine = limit - outstanding
		if creditLine < 0 {
			creditLine = 0
		}
	}
	return available, creditLine, outstanding, nil
}
