package admission

import (
	"context"
	"strings"
	"time"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
)

// SpendgatePolicyLoader resolves the payer's policy windows inside the
// admission transaction. FX conversion expresses each limit in the request unit.
type SpendgatePolicyLoader struct {
	policies *BillingPolicyStore
	fx       fx.Provider
}

// NewSpendgatePolicyLoader wires the loader.
func NewSpendgatePolicyLoader(policies *BillingPolicyStore, fxp fx.Provider) *SpendgatePolicyLoader {
	return &SpendgatePolicyLoader{policies: policies, fx: fxp}
}

// ResolvePolicy reads the current binding and policy together from PostgreSQL.
func (l *SpendgatePolicyLoader) ResolvePolicy(ctx context.Context, payer identity.CustomerID, trustLevel string) (ResolvedPolicy, error) {
	return l.policies.Resolve(ctx, payer, trustLevel)
}

// Load turns an already-resolved policy into the payer-scope windows the SQL
// gate enforces, in requestCurrency. Only window_spend_cap declares NEW-spend
// windows; an outstanding_cap policy caps debt, not velocity.
func (l *SpendgatePolicyLoader) Load(ctx context.Context, requestCurrency string, resolved ResolvedPolicy) (spendgate.Policy, error) {
	pw, err := l.convert(ctx, spendgate.ScopePayer, resolved.SpendWindows, requestCurrency)
	if err != nil {
		return spendgate.Policy{}, err
	}
	if len(pw) == 0 {
		return spendgate.Policy{}, nil
	}
	return spendgate.Policy{Scopes: []spendgate.ScopedWindows{{Scope: spendgate.ScopePayer, Windows: pw}}}, nil
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
// In-flight request reservations are already included in capacity.Held.
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
func payerCapacity(capacity money.AdmissionCapacity, policy ResolvedPolicy) (available, creditLine, outstanding int64) {
	available = capacity.Balance - capacity.Held
	outstanding = capacity.OutstandingOwed
	if capacity.BillingMode != money.BillingModeArrears {
		// Prepaid: money already owed is not available to new work.
		return available - outstanding, 0, outstanding
	}
	limit := capacity.CreditLimit
	if policy.OutstandingCapAmount > 0 {
		limit = policy.OutstandingCapAmount
	}
	if limit <= 0 {
		return available, 0, outstanding
	}
	creditLine = limit
	if policy.GatesOnOutstandingOwed() {
		creditLine = limit - outstanding
		if creditLine < 0 {
			creditLine = 0
		}
	}
	return available, creditLine, outstanding
}
