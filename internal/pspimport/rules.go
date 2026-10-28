package pspimport

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Fact names every rail stages. A rail may add its own (metadata.<key>).
const (
	FactEmail           = "email"
	FactUsername        = "username"
	FactName            = "name"
	FactOrderID         = "order_id"
	FactCustomerRef     = "customer_ref"
	FactSubscriptionRef = "subscription_ref"
	FactBillingID       = "billing_id"
	FactShippingID      = "shipping_id"
	FactPlan            = "plan"
)

var (
	factName    = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[A-Za-z0-9_-]+)?$`)
	mappingName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

// maxRules bounds an import's rules.
const maxRules = 20

// rule is a validated rule with its pattern compiled.
type rule struct {
	billing.PSPImportRule
	pattern *regexp.Regexp
}

// key is the lookup key a fact value yields, "" when the pattern refuses it.
func (r rule) key(value string) string {
	value = strings.TrimSpace(value)
	if r.pattern == nil {
		return value
	}
	m := r.pattern.FindStringSubmatch(value)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// compileRules checks the merchant's rules; nil is the default.
func compileRules(in []billing.PSPImportRule) ([]rule, error) {
	if in == nil {
		in = billing.DefaultPSPImportRules()
	}
	if len(in) > maxRules {
		return nil, apperr.Invalidf("at most %d rules", maxRules).WithParam("rules")
	}
	out := make([]rule, 0, len(in))
	for i, r := range in {
		param := fmt.Sprintf("rules[%d]", i)
		if !factName.MatchString(r.Fact) {
			return nil, apperr.Invalidf("%s.fact %q is not a fact name", param, r.Fact).WithParam(param + ".fact")
		}
		switch r.Via {
		case billing.PSPImportViaDirectory:
			if r.Fact != FactEmail && r.Fact != FactUsername {
				return nil, apperr.Invalidf("%s: the directory holds emails and usernames only", param).WithParam(param + ".via")
			}
			if r.Mapping != "" {
				return nil, apperr.Invalidf("%s: mapping goes with via mapping", param).WithParam(param + ".mapping")
			}
		case billing.PSPImportViaMapping:
			if !mappingName.MatchString(r.Mapping) {
				return nil, apperr.Invalidf("%s.mapping %q is not a mapping name", param, r.Mapping).WithParam(param + ".mapping")
			}
		case billing.PSPImportViaCustomerID:
			if r.Mapping != "" {
				return nil, apperr.Invalidf("%s: mapping goes with via mapping", param).WithParam(param + ".mapping")
			}
		default:
			return nil, apperr.Invalidf("%s.via must be directory, mapping or customer_id", param).WithParam(param + ".via")
		}
		compiled := rule{PSPImportRule: r}
		if r.Pattern != "" {
			before, after, ok := strings.Cut(r.Pattern, "{key}")
			if !ok || strings.Contains(after, "{key}") {
				return nil, apperr.Invalidf("%s.pattern holds exactly one {key}", param).WithParam(param + ".pattern")
			}
			compiled.pattern = regexp.MustCompile("^" + regexp.QuoteMeta(before) + "(.+)" + regexp.QuoteMeta(after) + "$")
		}
		out = append(out, compiled)
	}
	return out, nil
}

// facts are a staged object's identity facts: each name's distinct values.
type facts map[string][]string

func (f facts) add(name string, values ...string) {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if name == FactEmail {
			v = strings.ToLower(v)
		}
		if !slices.Contains(f[name], v) {
			f[name] = append(f[name], v)
		}
	}
}

// customerIDKey reads a customer id fact.
func customerIDKey(key string) (uuid.UUID, bool) {
	id, err := uuid.Parse(key)
	return id, err == nil && id != uuid.Nil
}
