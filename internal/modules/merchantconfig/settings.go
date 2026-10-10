package merchantconfig

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
)

// Invoice period boundaries a merchant's settings may name.
const (
	InvoiceBoundaryCalendarMonth = "calendar_month"
	InvoiceBoundaryFixedInterval = "fixed_interval"
	InvoiceBoundaryAnniversary   = "anniversary"
)

// NormalizeInvoiceBoundary canonicalizes a billing_period_boundary; "" is
// fixed_interval and an unknown value "".
func NormalizeInvoiceBoundary(boundary string) string {
	switch strings.ToLower(strings.TrimSpace(boundary)) {
	case "", InvoiceBoundaryFixedInterval:
		return InvoiceBoundaryFixedInterval
	case InvoiceBoundaryCalendarMonth:
		return InvoiceBoundaryCalendarMonth
	case InvoiceBoundaryAnniversary:
		return InvoiceBoundaryAnniversary
	default:
		return ""
	}
}

// Settings is a merchant's settings, validated: the configuration readers
// use, its named billing policies, which tier each binds and the default.
type Settings struct {
	Config   models.MerchantConfiguration
	Policies map[string]models.BillingPolicy
	// Tiers maps a tier to the name of its policy.
	Tiers map[string]string
	// Default names the policy of a customer no tier or assignment binds;
	// "" none.
	Default string
}

// Normalize validates a merchant's settings document as every writer and
// reader sees it. displayName is the merchant document's own name.
func Normalize(displayName string, in billing.MerchantSettings) (Settings, error) {
	out := Settings{Policies: map[string]models.BillingPolicy{}, Tiers: map[string]string{}}
	cfg := &out.Config
	cfg.Profile.DisplayName = strings.TrimSpace(displayName)
	if p := in.Profile; p != nil {
		for field, raw := range map[string]string{"logo_url": p.LogoURL, "support_url": p.SupportURL, "signup_url": p.SignupURL} {
			if raw = strings.TrimSpace(raw); raw != "" && !httpURL(raw) {
				return Settings{}, fmt.Errorf("profile.%s must be an http or https URL", field)
			}
		}
		cfg.Profile.LogoURL, cfg.Profile.FromEmail = strings.TrimSpace(p.LogoURL), strings.TrimSpace(p.FromEmail)
		cfg.Profile.SupportURL, cfg.Profile.SignupURL = strings.TrimSpace(p.SupportURL), strings.TrimSpace(p.SignupURL)
	}
	for name, v := range map[string]*int64{"collection_threshold": in.InvoiceCollectionThreshold, "monthly_floor": in.InvoiceMonthlyFloor, "arrears_delinquency_floor": in.ArrearsDelinquencyFloor} {
		if v != nil && *v < 0 {
			return Settings{}, fmt.Errorf("%s must be >= 0", name)
		}
	}
	for name, v := range map[string]*int{"reprice_notice_window_days": in.RepriceNoticeWindowDays, "renewal_receipt_min_interval_hours": in.RenewalReceiptMinIntervalHours, "arrears_grace_days": in.ArrearsGraceDays} {
		if v != nil && *v < 0 {
			return Settings{}, fmt.Errorf("%s must be >= 0", name)
		}
	}
	cfg.InvoiceCollectionThreshold, cfg.InvoiceMonthlyFloor, cfg.ArrearsDelinquencyFloor = in.InvoiceCollectionThreshold, in.InvoiceMonthlyFloor, in.ArrearsDelinquencyFloor
	cfg.RepriceNoticeWindowDays, cfg.RenewalReceiptMinIntervalHours, cfg.ArrearsGraceDays = in.RepriceNoticeWindowDays, in.RenewalReceiptMinIntervalHours, in.ArrearsGraceDays
	if in.InvoiceBillingBoundary != "" {
		if NormalizeInvoiceBoundary(in.InvoiceBillingBoundary) == "" {
			return Settings{}, fmt.Errorf("invalid billing_period_boundary %q", in.InvoiceBillingBoundary)
		}
		cfg.InvoiceBillingBoundary = in.InvoiceBillingBoundary
	}
	if in.AlertEmail != nil {
		cfg.AlertEmail = strings.TrimSpace(*in.AlertEmail)
	}
	if in.ProviderRefundAccess != nil {
		policy, err := NormalizeProviderRefundAccess(strings.TrimSpace(*in.ProviderRefundAccess))
		if err != nil {
			return Settings{}, err
		}
		cfg.ProviderRefundAccess = policy
	}
	if in.DunningPolicy != nil {
		if _, err := collection.PolicyOf(in.DunningPolicy); err != nil {
			return Settings{}, err
		}
		cfg.DunningPolicy = in.DunningPolicy
	}
	if in.CheckoutRouting != nil {
		routing, err := NormalizeCheckoutRouting(*in.CheckoutRouting)
		if err != nil {
			return Settings{}, err
		}
		cfg.CheckoutRouting = routing
	}
	windows, err := NormalizeBudgetWindows("merchant settings", "delegated_invoker_wasted_spend_limits", budgetWindows(in.DelegatedInvokerWastedSpendLimits))
	if err != nil {
		return Settings{}, err
	}
	for _, w := range windows {
		if w.WindowSeconds > 0 {
			cfg.DelegatedInvokerWastedSpendWindows = append(cfg.DelegatedInvokerWastedSpendWindows, w)
		}
	}
	for _, policy := range in.BillingPolicies {
		name, body, err := NormalizeDeclaredBillingPolicy(policy)
		if err != nil {
			return Settings{}, err
		}
		if _, exists := out.Policies[name]; exists {
			return Settings{}, fmt.Errorf("duplicate billing policy %q", name)
		}
		out.Policies[name] = body
	}
	bound := map[string]bool{}
	for _, binding := range in.BillingPolicyBindings {
		tier, name := strings.TrimSpace(binding.Tier), strings.TrimSpace(binding.PolicyName)
		if _, exists := out.Policies[name]; !exists {
			return Settings{}, fmt.Errorf("binding names undeclared policy %q", name)
		}
		if bound[tier] {
			return Settings{}, fmt.Errorf("duplicate binding for tier %q", tier)
		}
		bound[tier] = true
		if tier == "" {
			out.Default = name
		} else {
			out.Tiers[tier] = name
		}
	}
	return out, nil
}

// NormalizeDeclaredBillingPolicy validates one declared billing policy and
// returns its canonical name and body.
func NormalizeDeclaredBillingPolicy(in billing.BillingPolicy) (string, models.BillingPolicy, error) {
	name, err := NormalizeBillingPolicyName(in.Name)
	if err != nil {
		return "", models.BillingPolicy{}, err
	}
	body, err := NormalizeBillingPolicy(name, models.BillingPolicy{
		Kind:                      models.BillingPolicyKind(in.Kind),
		OutstandingCapAmount:      in.OutstandingCapAmount,
		SpendWindows:              budgetWindows(in.SpendWindows),
		AccrualRateCapPerHour:     in.AccrualRateCapPerHour,
		AccrualRateWindowSeconds:  in.AccrualRateWindowSeconds,
		BadSpendWindows:           budgetWindows(in.BadSpendWindows),
		CollectionThresholdAmount: in.CollectionThresholdAmount,
		CollectionCycleBoundary:   in.CollectionCycleBoundary,
		DelinquencyGraceDays:      in.DelinquencyGraceDays,
		DelinquencyAmountFloor:    in.DelinquencyAmountFloor,
		PolicyCurrency:            in.PolicyCurrency,
	})
	return name, body, err
}

func budgetWindows(ws []billing.BudgetWindow) []models.BudgetWindowPolicy {
	out := make([]models.BudgetWindowPolicy, 0, len(ws))
	for _, w := range ws {
		out = append(out, models.BudgetWindowPolicy{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency})
	}
	return out
}

func httpURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Policy resolves a customer's billing policy: its own assignment while the
// settings still declare it, else its tier's, else the default. dangling
// reports an assignment the settings no longer declare.
func (s Settings) Policy(assigned, tier string) (name string, body models.BillingPolicy, ok, dangling bool) {
	if assigned != "" {
		if body, ok := s.Policies[assigned]; ok {
			return assigned, body, true, false
		}
		dangling = true
	}
	for _, candidate := range []string{s.Tiers[strings.TrimSpace(tier)], s.Default} {
		if body, ok := s.Policies[candidate]; ok && candidate != "" {
			return candidate, body, true, dangling
		}
	}
	return "", models.BillingPolicy{}, false, dangling
}

// PoliciesParam is the settings' policies as the per-merchant SQL reads them:
// {policies: {name: body}, tiers: {tier: name}, default: name}.
func (s Settings) PoliciesParam() []byte {
	body, _ := json.Marshal(struct {
		Policies map[string]models.BillingPolicy `json:"policies"`
		Tiers    map[string]string               `json:"tiers"`
		Default  *string                         `json:"default"`
	}{s.Policies, s.Tiers, nonEmpty(s.Default)})
	return body
}

func nonEmpty(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
