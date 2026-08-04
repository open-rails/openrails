package merchantconfig

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
)

// Checkout routing policy shape (or#288). ONE validator serves both declaration
// paths — the mode-1 manifest and the mode-2 config API — so a policy that
// boots cannot be a policy the API would have refused.

var validRoutingModes = map[string]struct{}{
	string(models.CheckoutSessionModeOneOff):       {},
	string(models.CheckoutSessionModeSubscription): {},
}

// NormalizeCheckoutRouting validates a declared policy and returns it in
// canonical form (lowercased selectors and conditions). It rejects rather than
// repairs: a rule that cannot mean what it says is an operator error, and
// silently dropping it would route money somewhere nobody chose.
func NormalizeCheckoutRouting(rules []models.CheckoutRoutingRule) ([]models.CheckoutRoutingRule, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make([]models.CheckoutRoutingRule, 0, len(rules))
	catchAll := -1
	for i, rule := range rules {
		if catchAll >= 0 {
			return nil, fmt.Errorf("checkout_routing[%d] is unreachable: rule %d matches everything", i, catchAll)
		}
		match, err := normalizeRoutingMatch(i, rule.Match)
		if err != nil {
			return nil, err
		}
		prefer := make([]string, 0, len(rule.Prefer))
		seen := make(map[string]struct{}, len(rule.Prefer))
		for _, selector := range rule.Prefer {
			selector = strings.ToLower(strings.TrimSpace(selector))
			if selector == "" {
				return nil, fmt.Errorf("checkout_routing[%d].prefer contains an empty selector", i)
			}
			if _, dup := seen[selector]; dup {
				return nil, fmt.Errorf("checkout_routing[%d].prefer repeats %q", i, selector)
			}
			seen[selector] = struct{}{}
			prefer = append(prefer, selector)
		}
		if len(prefer) == 0 {
			// An empty prefer list would route nothing at all. A merchant that
			// wants a rail off archives the PSP; it does not declare a dead rule.
			return nil, fmt.Errorf("checkout_routing[%d].prefer must name at least one PSP", i)
		}
		if match.IsCatchAll() {
			catchAll = i
		}
		out = append(out, models.CheckoutRoutingRule{Match: match, Prefer: prefer})
	}
	return out, nil
}

func normalizeRoutingMatch(i int, m models.CheckoutRoutingMatch) (models.CheckoutRoutingMatch, error) {
	out := models.CheckoutRoutingMatch{
		Currency: strings.ToLower(strings.TrimSpace(m.Currency)),
		Product:  strings.TrimSpace(m.Product),
		Price:    strings.TrimSpace(m.Price),
		Mode:     strings.ToLower(strings.TrimSpace(m.Mode)),
		Country:  strings.ToUpper(strings.TrimSpace(m.Country)),
	}
	if out.Currency != "" && !isAlpha(out.Currency, 3) {
		return out, fmt.Errorf("checkout_routing[%d].match.currency must be a 3-letter ISO-4217 code", i)
	}
	if out.Country != "" && !isAlpha(out.Country, 2) {
		return out, fmt.Errorf("checkout_routing[%d].match.country must be a 2-letter ISO-3166-1 code", i)
	}
	if out.Mode != "" {
		if _, ok := validRoutingModes[out.Mode]; !ok {
			return out, fmt.Errorf("checkout_routing[%d].match.mode must be one_off or subscription", i)
		}
	}
	return out, nil
}

func isAlpha(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}
