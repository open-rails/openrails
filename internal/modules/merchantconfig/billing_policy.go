package merchantconfig

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Billing-policy shape (or#897). ONE validator serves both declaration paths —
// the mode-1 manifest and the mode-2 config API — so a policy that boots cannot
// be a policy the API would have refused, and vice versa. Same contract as
// NormalizeCheckoutRouting (or#288): it REJECTS rather than repairs, because a
// silently-dropped limit is a spend cap nobody chose.

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

	kind := models.BillingPolicyKind(strings.ToLower(strings.TrimSpace(string(p.Kind))))
	switch kind {
	case models.BillingPolicyOutstandingCap, models.BillingPolicyWindowSpendCap:
	case models.BillingPolicyAccrualRateCap:
		// Representable so the vocabulary is complete and the refusal is a real
		// answer instead of "unknown kind". or#897 PR 3 builds the rate measurement.
		return models.BillingPolicy{}, fmt.Errorf("%s: kind %q is not implemented yet (or#897 PR 3)", label, kind)
	case "":
		return models.BillingPolicy{}, fmt.Errorf("%s: kind is required (outstanding_cap or window_spend_cap)", label)
	default:
		return models.BillingPolicy{}, fmt.Errorf("%s: unknown kind %q", label, kind)
	}
	out := models.BillingPolicy{Kind: kind}

	currency, err := normalizePolicyCurrency(label, "policy_currency", p.PolicyCurrency)
	if err != nil {
		return models.BillingPolicy{}, err
	}
	out.PolicyCurrency = currency

	// Each kind accepts only ITS limit. Accepting the other kind's field would
	// store a cap that is never read — a knob that looks enforced and is not.
	switch kind {
	case models.BillingPolicyOutstandingCap:
		if len(p.SpendWindows) > 0 {
			return models.BillingPolicy{}, fmt.Errorf("%s: spend_windows belong to kind window_spend_cap", label)
		}
		if p.OutstandingCapAmount < 0 {
			return models.BillingPolicy{}, fmt.Errorf("%s: outstanding_cap_amount must be non-negative", label)
		}
		out.OutstandingCapAmount = p.OutstandingCapAmount
	case models.BillingPolicyWindowSpendCap:
		if p.OutstandingCapAmount != 0 {
			return models.BillingPolicy{}, fmt.Errorf("%s: outstanding_cap_amount belongs to kind outstanding_cap", label)
		}
		if len(p.SpendWindows) == 0 {
			return models.BillingPolicy{}, fmt.Errorf("%s: kind window_spend_cap requires at least one spend_windows entry", label)
		}
		windows, err := normalizeBillingWindows(label, "spend_windows", p.SpendWindows)
		if err != nil {
			return models.BillingPolicy{}, err
		}
		out.SpendWindows = windows
	}

	// Wasted-spend grace is orthogonal to the capped quantity, so it rides on
	// either kind.
	badSpend, err := normalizeBillingWindows(label, "bad_spend_windows", p.BadSpendWindows)
	if err != nil {
		return models.BillingPolicy{}, err
	}
	out.BadSpendWindows = badSpend
	return out, nil
}

func normalizeBillingWindows(label, field string, in []models.BudgetWindowPolicy) ([]models.BudgetWindowPolicy, error) {
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
		if w.WindowSeconds <= 0 {
			return nil, fmt.Errorf("%s: %s[%d].window_seconds must be positive", label, field, i)
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

// BillingPolicyBindingRung identifies which rung a binding sits on.
type BillingPolicyBindingRung string

const (
	// BindingRungDefault is the merchant-wide fallback (no customer, no tier).
	BindingRungDefault BillingPolicyBindingRung = "default"
	// BindingRungTier applies to every payer at one trust tier.
	BindingRungTier BillingPolicyBindingRung = "tier"
	// BindingRungCustomer applies to exactly one payer and beats the other two.
	BindingRungCustomer BillingPolicyBindingRung = "customer"
)

// NormalizeBillingPolicyBinding validates one binding's rung and returns the
// canonical policy name, tier and rung. customerSet reports whether a customer
// id was supplied; the caller owns parsing it, since the two transports carry
// it differently (uuid on the wire, typed id in-process).
func NormalizeBillingPolicyBinding(policyName, tier string, customerSet bool) (name string, canonicalTier string, rung BillingPolicyBindingRung, err error) {
	name, err = NormalizeBillingPolicyName(policyName)
	if err != nil {
		return "", "", "", err
	}
	canonicalTier = strings.TrimSpace(tier)
	switch {
	case customerSet && canonicalTier != "":
		// Most-specific-wins cannot rank a binding that is both, so the shape
		// forbids it rather than silently preferring one.
		return "", "", "", fmt.Errorf("billing policy binding %q: bind to a customer OR a tier, not both", name)
	case customerSet:
		return name, "", BindingRungCustomer, nil
	case canonicalTier != "":
		return name, canonicalTier, BindingRungTier, nil
	default:
		return name, "", BindingRungDefault, nil
	}
}
