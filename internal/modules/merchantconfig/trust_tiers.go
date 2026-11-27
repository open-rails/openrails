package merchantconfig

import (
	"fmt"
	"slices"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TrustRule is one trust tier's requirement in one currency, as
// billing.trust_tier reads it: the tier, the net amount paid and the 24-hour
// days since the first payment it asks.
type TrustRule struct {
	Tier          string `json:"tier"`
	NetPaidAmount int64  `json:"net_paid_amount"`
	Days          int    `json:"days"`
}

// DefaultTrustLevel is the tier of a customer with no explicit trust level
// that meets no trust tier's requirement.
const DefaultTrustLevel = "free"

const (
	// maxTrustTiers bounds the tiers a merchant declares.
	maxTrustTiers = 32
	// maxTrustTierBytes bounds a tier's name, as a customer's trust level is.
	maxTrustTierBytes = 64
	// maxTrustTierDays bounds days_since_first_payment: ten years.
	maxTrustTierDays = 3650
)

// NormalizeTrustTiers validates the merchant's trust tiers against its
// bindings (tier to policy) and returns each currency's rules, highest tier
// first.
func NormalizeTrustTiers(in []billing.TrustTier, bound map[string]string) (map[string][]TrustRule, error) {
	if len(in) > maxTrustTiers {
		return nil, fmt.Errorf("trust_tiers declares %d tiers; at most %d", len(in), maxTrustTiers)
	}
	rules := map[string][]TrustRule{}
	named := map[string]bool{}
	for i, tier := range in {
		field := fmt.Sprintf("trust_tiers[%d]", i)
		name := strings.TrimSpace(tier.Name)
		switch {
		case name == "":
			return nil, fmt.Errorf("%s.name is required", field)
		case len(name) > maxTrustTierBytes:
			return nil, fmt.Errorf("%s.name exceeds %d bytes", field, maxTrustTierBytes)
		case name == DefaultTrustLevel:
			return nil, fmt.Errorf("%s.name %q is the default tier: a customer is at it until it meets a tier's requirement", field, name)
		case named[name]:
			return nil, fmt.Errorf("trust_tiers names %q twice", name)
		case bound[name] == "":
			return nil, fmt.Errorf("trust tier %q has no billing_policy_bindings entry: bind it a policy, or reaching it changes nothing", name)
		case len(tier.Requirements) == 0:
			return nil, fmt.Errorf("%s (%s) has no requirements", field, name)
		}
		named[name] = true
		seen := map[string]bool{}
		for j, req := range tier.Requirements {
			at := fmt.Sprintf("%s.requirements[%d]", field, j)
			code := moneyutil.NormalizeCurrency(string(req.Currency))
			units, ok := moneyutil.LookupCurrency(code)
			if !ok {
				return nil, fmt.Errorf("%s.currency: unknown currency %q", at, req.Currency)
			}
			if seen[code] {
				return nil, fmt.Errorf("%s (%s) names %s twice", field, name, code)
			}
			seen[code] = true
			if req.NetPaidAmount < 0 || req.DaysSinceFirstPayment < 0 || req.DaysSinceFirstPayment > maxTrustTierDays {
				return nil, fmt.Errorf("%s: net_paid_amount must be >= 0 and days_since_first_payment 0 to %d", at, maxTrustTierDays)
			}
			if req.NetPaidAmount == 0 && req.DaysSinceFirstPayment == 0 {
				return nil, fmt.Errorf("%s asks nothing: name net_paid_amount or days_since_first_payment", at)
			}
			if step := minorUnit(units); req.NetPaidAmount%step != 0 {
				return nil, fmt.Errorf("%s.net_paid_amount %d is not whole minor units of %s: amounts are in its native units (%s is %d)",
					at, req.NetPaidAmount, code, moneyutil.FormatAmount(step, code), step)
			}
			rule := TrustRule{Tier: name, NetPaidAmount: req.NetPaidAmount, Days: req.DaysSinceFirstPayment}
			if below := rules[code]; len(below) > 0 {
				prev := below[len(below)-1]
				if rule.NetPaidAmount < prev.NetPaidAmount || rule.Days < prev.Days || rule.NetPaidAmount == prev.NetPaidAmount && rule.Days == prev.Days {
					return nil, fmt.Errorf("%s (%s) asks no more than %s in %s: a tier asks at least what the tier below it asks, and more of one criterion", field, name, prev.Tier, code)
				}
			}
			rules[code] = append(rules[code], rule)
		}
	}
	for code := range rules {
		slices.Reverse(rules[code])
	}
	return rules, nil
}

// minorUnit is one minor unit of a currency in its native units: 10000 for
// USD's micros.
func minorUnit(units moneyutil.Currency) int64 {
	step := int64(1)
	for range units.NativeShift() {
		step *= 10
	}
	return step
}
