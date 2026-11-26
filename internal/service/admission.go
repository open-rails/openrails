package service

import (
	"cmp"
	"context"
	"time"

	"github.com/open-rails/openrails/billing"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
)

// BillingPolicy declares one named billing policy (or#897). Window entries
// carry the same {key, window_seconds, limit, currency} shape everywhere in this
// package — billing.BudgetWindow.
type BillingPolicy = billing.BillingPolicy

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
// bound billing policy's bad_spend windows (#488) at the payer's current tier;
// a window that names no currency is in the policy's policy_currency.
func (s *Service) payerWastedWindows(ctx context.Context, payer identity.CustomerID, currency, trustLevel string) ([]abuse.WastedWindow, error) {
	if trustLevel == "" {
		level, err := s.moneyService().TrustLevel(ctx, payer, currency)
		if err != nil {
			return nil, err
		}
		trustLevel = level.TrustLevel
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
		out = append(out, abuse.WastedWindow{Key: w.Key, Window: time.Duration(w.WindowSeconds) * time.Second, Limit: w.Limit, Currency: cmp.Or(w.Currency, pol.PolicyCurrency)})
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
	unit := money.IsMerchantUnit(currency)
	for _, w := range windows {
		if w.Limit <= 0 || w.Window < time.Second {
			continue
		}
		limit := w.Limit
		wc := money.NormalizeCurrency(w.Currency)
		// No rate converts money into a merchant's unit, and a limit naming no
		// currency (the defaults among them) is money.
		if unit && wc != currency {
			continue
		}
		if wc != "" && wc != currency {
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
