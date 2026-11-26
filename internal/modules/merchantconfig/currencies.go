package merchantconfig

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// NormalizeCurrencies validates the merchant's own units and returns its
// currency registry: OpenRails' with them.
func NormalizeCurrencies(in []billing.MerchantCurrency) (currency.Registry, error) {
	units := make([]currency.Units, 0, len(in))
	for i, c := range in {
		u, err := currency.Unit(string(c.Code), c.Name, c.Symbol, c.Scale)
		if err != nil {
			return currency.Registry{}, fmt.Errorf("currencies[%d]: %w", i, err)
		}
		units = append(units, u)
	}
	registry, err := currency.NewRegistry(units)
	if err != nil {
		return currency.Registry{}, fmt.Errorf("currencies: %w", err)
	}
	return registry, nil
}

// Currencies are a registry's units as the wire shows them, in code order.
func Currencies(r currency.Registry) []billing.MerchantCurrency {
	units := r.Units()
	out := make([]billing.MerchantCurrency, 0, len(units))
	for _, u := range units {
		out = append(out, billing.MerchantCurrency{Code: catalog.Currency(u.Code), Name: u.Name, Symbol: u.Symbol, Scale: u.Decimals})
	}
	return out
}

// UnitsDoc names, among a merchant's refused documents, the units a loaded
// document removed or rescaled while something counts them: their previous
// declaration keeps serving.
const UnitsDoc = "currencies"

// UnitsInUse are the merchant's units something counts: a balance, a price, a
// product's credit or a rate card.
func UnitsInUse(ctx context.Context, q *gen.Queries, merchant uuid.UUID) ([]string, error) {
	return q.ListMerchantUnitsInUse(ctx, gen.ListMerchantUnitsInUseParams{MerchantID: merchant, Registry: moneyutil.CurrencyCodes()})
}

// CountedScales are the scales the units book recorded the merchant's units
// at, by code.
func CountedScales(ctx context.Context, q *gen.Queries, merchant uuid.UUID) (map[string]int, error) {
	rows, err := q.ListLedgerUnits(ctx, merchant)
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.Unit] = int(r.Scale)
	}
	return out, err
}

// ChangedUnits are the units held declares that next removes or rescales.
func ChangedUnits(held []billing.MerchantCurrency, next currency.Registry) []string {
	var out []string
	for _, u := range held {
		code := strings.ToUpper(strings.TrimSpace(string(u.Code)))
		if kept, ok := next.Unit(code); !ok || kept.Decimals != u.Scale {
			out = append(out, code)
		}
	}
	return out
}

// KeepCountedUnits keeps, in a loaded document, each unit in use as it is
// counted, and answers why; "" when the document keeps every one. A unit the
// units book recorded (counted, by code) keeps its recorded scale, so a
// restart is no way around it, and one the document drops keeps serving at
// it, named by its code: configuration is authored in a file or Vault, but a
// unit's scale is what its stored amounts mean, ledger evidence the ledger
// may supply. Another unit in use keeps the declaration served before
// (previous), and one no declaration served is named alone.
func KeepCountedUnits(inUse []string, counted map[string]int, previous []billing.MerchantCurrency, doc *billing.MerchantSettings) string {
	served := map[string]billing.MerchantCurrency{}
	for _, u := range previous {
		served[strings.ToUpper(string(u.Code))] = u
	}
	currencies := slices.Clone(doc.Currencies)
	declared := map[string]int{}
	for i, u := range currencies {
		declared[strings.ToUpper(string(u.Code))] = i
	}
	var why []string
	for _, code := range inUse {
		i, ok := declared[code]
		old, kept := served[code]
		scale, recorded := counted[code]
		if recorded {
			// The served declaration names and shows the unit; the book's
			// scale is what its amounts were written at.
			if !kept || old.Scale != scale {
				old, kept = billing.MerchantCurrency{Code: catalog.Currency(code), Name: code, Scale: scale}, true
				if ok {
					old.Name, old.Symbol = currencies[i].Name, currencies[i].Symbol
				}
			}
		}
		switch {
		case ok && (!kept || currencies[i].Scale == old.Scale):
		case ok:
			currencies[i] = old
			why = append(why, code+" rescaled")
		case kept:
			currencies = append(currencies, old)
			why = append(why, code+" removed")
		default:
			why = append(why, code+" undeclared")
		}
	}
	if len(why) == 0 {
		return ""
	}
	doc.Currencies = currencies
	return "units that balances, prices, credit or rate cards count keep their declaration (" + strings.Join(why, ", ") + "); declare them as they were"
}

// refuseUnits refuses (merchant_unit_unsupported) settings that name one of
// the merchant's units where they take money: usage billing and its arrears,
// billing policies, spend limits, PSP routing and low-balance lines.
func refuseUnits(in billing.MerchantSettings, r currency.Registry) error {
	var fields []string
	var codes []catalog.Currency
	add := func(field string, code catalog.Currency) {
		fields, codes = append(fields, field), append(codes, code)
	}
	if in.UsageBilling != nil {
		for i, c := range in.UsageBilling.Currencies {
			add(fmt.Sprintf("usage_billing.currencies[%d].currency", i), c.Currency)
		}
	}
	for i, p := range in.BillingPolicies {
		add(fmt.Sprintf("billing_policies[%d].policy_currency", i), p.PolicyCurrency)
		for j, w := range p.SpendWindows {
			add(fmt.Sprintf("billing_policies[%d].spend_windows[%d].currency", i, j), w.Currency)
		}
		for j, w := range p.BadSpendWindows {
			add(fmt.Sprintf("billing_policies[%d].bad_spend_windows[%d].currency", i, j), w.Currency)
		}
	}
	for i, w := range in.DelegatedInvokerWastedSpendLimits {
		add(fmt.Sprintf("delegated_invoker_wasted_spend_limits[%d].currency", i), w.Currency)
	}
	if in.PSPRouting != nil {
		for i, rule := range *in.PSPRouting {
			add(fmt.Sprintf("psp_routing[%d].match.currency", i), rule.Match.Currency)
		}
	}
	if in.BalanceNotices != nil {
		for i, line := range in.BalanceNotices.LowAmounts {
			add(fmt.Sprintf("balance_notices.low_amounts[%d].currency", i), line.Currency)
		}
	}
	for i, tier := range in.TrustTiers {
		for j, req := range tier.Requirements {
			add(fmt.Sprintf("trust_tiers[%d].requirements[%d].currency", i, j), req.Currency)
		}
	}
	for i, code := range codes {
		if code == "" {
			continue
		}
		if _, err := r.Money(string(code)); errors.Is(err, currency.ErrMerchantUnit) {
			return UnitRefusal(fields[i], err)
		}
	}
	return nil
}

// UnitRefusal is merchant_unit_unsupported for param, whose currency err
// found to be one of the merchant's units.
func UnitRefusal(param string, err error) error {
	return apperr.New(http.StatusBadRequest, billing.CodeMerchantUnitUnsupported,
		fmt.Sprintf("%s: %v; it takes a currency from OpenRails' registry", param, err)).WithParam(param)
}
