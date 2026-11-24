package merchantconfig

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/shared/apperr"
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
