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
// gate enforces) for one (payer, tier, request) from the Postgres tier_spend_caps +
// scoped_spend_caps. Every window's limit is expressed in the REQUEST currency
// (FX-converted when a window is denominated in another currency), so the gate
// runs in a single currency per admit. #513: this read replaces the per-request
// Postgres budget reservation; nothing on the hot path locks Postgres.
type SpendgatePolicyLoader struct {
	tiers   *TierSpendCapStore
	budgets *ScopedSpendCapStore
	fx      fx.Provider
	cache   *PolicyCache // optional; nil reads config from Postgres every load
}

// NewSpendgatePolicyLoader wires the loader. budgetScopes may be nil (then only
// the tier policy's payer-scope windows are loaded).
func NewSpendgatePolicyLoader(tiers *TierSpendCapStore, budgetScopes *ScopedSpendCapStore, fxp fx.Provider) *SpendgatePolicyLoader {
	return &SpendgatePolicyLoader{tiers: tiers, budgets: budgetScopes, fx: fxp}
}

// WithCache attaches the process-local policy-config cache (nil disables it; the
// loader then reads tier_spend_caps + scoped_spend_caps from Postgres every load).
func (l *SpendgatePolicyLoader) WithCache(c *PolicyCache) *SpendgatePolicyLoader {
	l.cache = c
	return l
}

// Load resolves the scoped windows for (payer, tier) in requestCurrency. req
// supplies the principals so the invoker_tier scope (whose policy key IS the tier)
// resolves to a per-invoker window that applies only at the matching tier.
//
// hasDelegatedGrant reports whether any invoker/role-scoped window matched the
// request — the caller uses it to deny a delegated invoker that has no explicit
// spend grant (the pre-existing "delegated_spend_not_allowed" guarantee).
func (l *SpendgatePolicyLoader) Load(ctx context.Context, payer identity.CustomerID, tier, requestCurrency string, req spendgate.Request) (pol spendgate.Policy, hasDelegatedGrant bool, err error) {
	var scopes []spendgate.ScopedWindows

	// Cache key needs the merchant so a payer uuid can't alias across merchants.
	tid, err := merchant.Require(ctx)
	if err != nil {
		return spendgate.Policy{}, false, err
	}
	mid := tid.UUID().String()
	payerID := payer.UUID().String()

	// Tier policy → payer-scope velocity windows (the payer's own cap at this tier).
	// Read-mostly config, served from the long-TTL policy cache when warm.
	tp, err := l.cache.TierSpendCaps(mid, payerID, tier, func() (TierSpendCaps, error) {
		return l.tiers.GetTierSpendCaps(ctx, payer, tier)
	})
	if err != nil {
		return spendgate.Policy{}, false, err
	}
	pw, err := l.convert(ctx, spendgate.ScopePayer, tp.BudgetWindows, requestCurrency)
	if err != nil {
		return spendgate.Policy{}, false, err
	}
	if len(pw) > 0 {
		scopes = append(scopes, spendgate.ScopedWindows{Scope: spendgate.ScopePayer, Windows: pw})
	}

	if l.budgets != nil {
		policies, lerr := l.cache.ScopedSpendCaps(mid, payerID, func() ([]ScopedSpendCap, error) {
			return l.budgets.LoadAll(ctx, payer)
		})
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
			case budgets.ScopeSubject:
				w, cerr := l.convert(ctx, spendgate.ScopePayer, bw, requestCurrency)
				if cerr != nil {
					return spendgate.Policy{}, false, cerr
				}
				if len(w) > 0 {
					scopes = append(scopes, spendgate.ScopedWindows{Scope: spendgate.ScopePayer, Windows: w})
				}
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
			case budgets.ScopeInvokerTier:
				// Per-invoker cap selected by tier: applies only at the matching tier.
				if strings.TrimSpace(p.ScopeKey) != strings.TrimSpace(tier) || strings.TrimSpace(req.Invoker) == "" {
					continue
				}
				w, cerr := l.convert(ctx, spendgate.ScopeInvoker, bw, requestCurrency)
				if cerr != nil {
					return spendgate.Policy{}, false, cerr
				}
				// Prefix the key so a tier-selected bucket can't alias a plain invoker window.
				for i := range w {
					w[i].Key = "it:" + tier + ":" + w[i].Key
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
		cad, err := spendgate.NormalizeCadence(w.Cadence)
		if err != nil {
			return nil, err
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
			Cadence:  cad,
			Duration: time.Duration(w.WindowSeconds) * time.Second,
			Limit:    limit,
			Key:      w.Key,
		})
	}
	return out, nil
}

// payerCapacity reads the (available, creditLine) figures the admit gate needs:
// available = settled balance − money-window reservations; creditLine = the
// arrears credit line (0 for prepaid), used as the gate's negative floor. This is
// an UNLOCKED read (no FOR UPDATE) — a stale read can only cause bounded
// over-admission, never a wrong durable charge (the #512 ledger is the truth).
func payerCapacity(ctx context.Context, moneySvc *money.MoneyService, payer identity.CustomerID, currency string) (available, creditLine int64, err error) {
	bal, err := moneySvc.GetBalanceForCustomer(ctx, payer, currency)
	if err != nil {
		return 0, 0, err
	}
	available = bal.Balance - bal.HeldBalance
	settings, err := moneySvc.GetAccountSettings(ctx, payer, currency)
	if err != nil {
		return 0, 0, err
	}
	if settings.BillingMode == money.BillingModeArrears {
		creditLine = settings.CreditLimitAmount - settings.OutstandingOwedAmount
		if creditLine < 0 {
			creditLine = 0
		}
	}
	return available, creditLine, nil
}
