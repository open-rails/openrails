package merchantconfig

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Billing-policy shape (or#897). ONE validator serves both declaration paths —
// the mode-1 manifest and the mode-2 config API — so a policy that boots cannot
// be a policy the API would have refused, and vice versa. Same contract as
// NormalizePSPRouting (or#288): it REJECTS rather than repairs, because a
// silently-dropped limit is a spend cap nobody chose.

// MaxSpendWindowSeconds is the longest spend window a policy or a spend
// delegation may declare. Admitted requests are kept for this long plus 30
// days, so a longer window would count requests already dropped.
const MaxSpendWindowSeconds = int64(retention.MaxSpendWindow / time.Second)

// MaxBillingPolicyNameLength bounds a policy name. Names are merchant-chosen
// identifiers that travel in manifests, bindings and API payloads, not prose.
const MaxBillingPolicyNameLength = 64

// NormalizeBillingPolicyName validates and canonicalizes a policy name.
func NormalizeBillingPolicyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("billing policy name is required")
	}
	if len(name) > MaxBillingPolicyNameLength {
		return "", fmt.Errorf("billing policy name %q exceeds %d characters", name, MaxBillingPolicyNameLength)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return "", fmt.Errorf("billing policy name %q may use only letters, digits, '_', '-' and '.'", name)
		}
	}
	return name, nil
}

// NormalizeBillingPolicy validates a declared policy body and returns it in
// canonical form. The error text names the policy so an operator with twenty
// declared policies is told which one is wrong.
func NormalizeBillingPolicy(name string, p models.BillingPolicy) (models.BillingPolicy, error) {
	label := strings.TrimSpace(name)
	if label == "" {
		label = "billing policy"
	} else {
		label = "billing policy " + label
	}

	currency, err := normalizePolicyCurrency(label, "policy_currency", p.PolicyCurrency)
	if err != nil {
		return models.BillingPolicy{}, err
	}
	out := models.BillingPolicy{PolicyCurrency: currency}

	if p.OutstandingCapAmount < 0 {
		return models.BillingPolicy{}, fmt.Errorf("%s: outstanding_cap_amount must be non-negative", label)
	}
	out.OutstandingCapAmount = p.OutstandingCapAmount
	windows, err := NormalizeBudgetWindows(label, "spend_windows", p.SpendWindows)
	if err != nil {
		return models.BillingPolicy{}, err
	}
	for i, w := range windows {
		if w.WindowSeconds > MaxSpendWindowSeconds {
			return models.BillingPolicy{}, fmt.Errorf("%s: spend_windows[%d].window_seconds must be at most %d (%d days)", label, i, MaxSpendWindowSeconds, MaxSpendWindowSeconds/86400)
		}
	}
	out.SpendWindows = windows

	if strings.TrimSpace(p.CollectionCycleBoundary) != "" {
		return models.BillingPolicy{}, fmt.Errorf(
			"%s: collection_cycle_boundary cannot be per-policy — a payer's statement periods must tile its lifetime with no gap or overlap, and rebinding is a live runtime lever, so a mid-cycle change would bill a stretch twice or never. Set invoice.billing_period_boundary on the merchant instead", label)
	}
	if p.CollectionThresholdAmount != nil {
		if *p.CollectionThresholdAmount < 0 {
			return models.BillingPolicy{}, fmt.Errorf("%s: collection_threshold_amount must be non-negative", label)
		}
		out.CollectionThresholdAmount = p.CollectionThresholdAmount
	}
	if p.DelinquencyGraceDays != nil {
		if *p.DelinquencyGraceDays < 0 {
			return models.BillingPolicy{}, fmt.Errorf("%s: delinquency_grace_days must be non-negative", label)
		}
		out.DelinquencyGraceDays = p.DelinquencyGraceDays
	}
	if p.DelinquencyAmountFloor != nil {
		if *p.DelinquencyAmountFloor < 0 {
			return models.BillingPolicy{}, fmt.Errorf("%s: delinquency_amount_floor must be non-negative", label)
		}
		// A floor is an amount of one currency: debt in another has its own.
		if out.PolicyCurrency == "" {
			return models.BillingPolicy{}, fmt.Errorf("%s: delinquency_amount_floor requires policy_currency, the currency it is in", label)
		}
		out.DelinquencyAmountFloor = p.DelinquencyAmountFloor
	}

	badSpend, err := NormalizeBudgetWindows(label, "bad_spend_windows", p.BadSpendWindows)
	if err != nil {
		return models.BillingPolicy{}, err
	}
	out.BadSpendWindows = badSpend
	return out, nil
}

// NormalizeBudgetWindows validates the shared merchant money-window shape.
func NormalizeBudgetWindows(label, field string, in []models.BudgetWindowPolicy) ([]models.BudgetWindowPolicy, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]models.BudgetWindowPolicy, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, w := range in {
		key := strings.TrimSpace(w.Key)
		if key == "" {
			return nil, fmt.Errorf("%s: %s[%d].key is required", label, field, i)
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s: %s repeats key %q", label, field, key)
		}
		seen[key] = struct{}{}
		if w.WindowSeconds <= 0 || w.WindowSeconds > math.MaxInt64/int64(time.Second) {
			return nil, fmt.Errorf("%s: %s[%d].window_seconds must be positive and representable as a duration", label, field, i)
		}
		if w.Limit < 0 {
			return nil, fmt.Errorf("%s: %s[%d].limit must be non-negative", label, field, i)
		}
		currency, err := normalizePolicyCurrency(label, fmt.Sprintf("%s[%d].currency", field, i), w.Currency)
		if err != nil {
			return nil, err
		}
		out = append(out, models.BudgetWindowPolicy{Key: key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: currency})
	}
	return out, nil
}

func normalizePolicyCurrency(label, field, value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if err := moneyutil.ValidateCurrency(value); err != nil {
		return "", fmt.Errorf("%s: %s invalid: %w", label, field, err)
	}
	return value, nil
}
