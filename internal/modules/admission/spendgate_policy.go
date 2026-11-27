package admission

import (
	"context"
	"strings"
	"time"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
)

// SpendgatePolicyLoader resolves the customer's policy windows inside the
// admission transaction. FX conversion expresses each limit in the request unit.
type SpendgatePolicyLoader struct {
	policies *BillingPolicyStore
	fx       *fx.Rates
}

// NewSpendgatePolicyLoader wires the loader.
func NewSpendgatePolicyLoader(policies *BillingPolicyStore, fxp *fx.Rates) *SpendgatePolicyLoader {
	return &SpendgatePolicyLoader{policies: policies, fx: fxp}
}

// ResolvePolicy reads the current binding and policy together from PostgreSQL.
func (l *SpendgatePolicyLoader) ResolvePolicy(ctx context.Context, customer identity.CustomerID, trustLevel string) (ResolvedPolicy, error) {
	return l.policies.Resolve(ctx, customer, trustLevel)
}

// Load turns an already-resolved policy into the customer-scope windows the SQL
// gate enforces, in requestCurrency. A window that names no currency is in
// the policy's policy_currency, else the request's.
func (l *SpendgatePolicyLoader) Load(ctx context.Context, requestCurrency string, resolved ResolvedPolicy) (spendgate.Policy, error) {
	pw, err := l.convert(ctx, spendgate.ScopeCustomer, resolved.SpendWindows, resolved.PolicyCurrency, requestCurrency)
	if err != nil {
		return spendgate.Policy{}, err
	}
	if len(pw) == 0 {
		return spendgate.Policy{}, nil
	}
	return spendgate.Policy{Scopes: []spendgate.ScopedWindows{{Scope: spendgate.ScopeCustomer, Windows: pw}}}, nil
}

// convert maps budgets.BudgetWindow → spendgate.Window, FX-converting the limit to
// requestCurrency when the window's currency (else policyCurrency) differs.
func (l *SpendgatePolicyLoader) convert(ctx context.Context, scope spendgate.Scope, ws []budgets.BudgetWindow, policyCurrency, requestCurrency string) ([]spendgate.Window, error) {
	reqCur := money.NormalizeCurrency(requestCurrency)
	out := make([]spendgate.Window, 0, len(ws))
	for _, w := range ws {
		if w.WindowSeconds <= 0 || w.Limit < 0 {
			continue
		}
		limit := w.Limit
		c := strings.TrimSpace(w.Currency)
		if c == "" {
			c = strings.TrimSpace(policyCurrency)
		}
		// A window naming no currency is money: it binds no unit.
		if currency.IsUnitCode(reqCur) && money.NormalizeCurrency(c) != reqCur {
			continue
		}
		if c != "" && money.NormalizeCurrency(c) != reqCur {
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

// customerCapacity is the affordability snapshot the spendgate is evaluated
// against: spendable balance (ledger customer_units counters, O(1)) less
// pending usage and running-charge accrual, plus, in arrears, the credit line
// still available: the customer's credit limit (its own, else its policy's
// cap, else the merchant's usage terms') less what it owes, floored at zero.
// Prepaid, what it owes is not available to new work.
func customerCapacity(capacity money.AdmissionCapacity) (available, creditLine, outstanding int64) {
	available = capacity.Balance - capacity.Unposted()
	outstanding = capacity.OutstandingOwed
	if capacity.BillingMode != money.BillingModeArrears {
		return available - outstanding, 0, outstanding
	}
	return available, max(capacity.CreditLimit-outstanding, 0), outstanding
}

// Available is what the customer may spend before any credit line: its
// balance less pending usage and accrual, less what it owes when it prepays.
func Available(capacity money.AdmissionCapacity) int64 {
	available, _, _ := customerCapacity(capacity)
	return available
}
