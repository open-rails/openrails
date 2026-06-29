package embedded

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/config"
)

// PaymentProvider is one payment-provider credential set supplied by an
// embedding host. Name is an optional local selector only; OpenRails durable
// identity comes from the provider account id resolved at runtime.
type PaymentProvider struct {
	// Name is an optional local config selector such as "stripe_primary" or
	// "mobius_legacy". Leave it empty when the host does not need to target the
	// credential set by name; OpenRails will generate a process-local selector.
	Name string
	// Config is the provider credential/config payload. Set Config.Type for
	// non-reserved names and for generated names. Config.Routing defaults to default.
	Config config.RailConfig
}

// ApplyPaymentProviders converts host-supplied embedded provider credentials
// into an in-memory RailSet. Local names are not durable identity; provider
// account rows store the provider-returned account id.
func ApplyPaymentProviders(providers []PaymentProvider) (config.RailSet, error) {
	set := config.RailSet{}
	if len(providers) == 0 {
		return set, nil
	}
	seen := map[string]struct{}{}
	for _, provider := range providers {
		proc := provider.Config
		key := strings.ToLower(strings.TrimSpace(provider.Name))
		providerType := strings.ToLower(strings.TrimSpace(proc.Type))
		if key != "" && providerType == "" {
			providerType = proc.GetEffectiveType(key)
		}
		if providerType == "" {
			return nil, fmt.Errorf("embedded payment provider %q requires config type", key)
		}
		if proc.Type == "" {
			proc.Type = providerType
		}
		if key == "" {
			key = generatedProviderName(set, seen, providerType)
		}
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate embedded payment provider name %q", key)
		}
		seen[key] = struct{}{}
		set[key] = cloneRailConfig(&proc)
	}
	return set, nil
}

func generatedProviderName(existing map[string]*config.RailConfig, seen map[string]struct{}, providerType string) string {
	base := strings.ToLower(strings.TrimSpace(providerType))
	if base == "" {
		base = "provider"
	}
	for i := 1; ; i++ {
		key := base
		if i > 1 {
			key = fmt.Sprintf("%s_%d", base, i)
		}
		if _, ok := existing[key]; ok {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		return key
	}
}

func cloneRailConfig(in *config.RailConfig) *config.RailConfig {
	if in == nil {
		return nil
	}
	out := *in
	if in.NMI != nil {
		v := *in.NMI
		out.NMI = &v
	}
	if in.CCBill != nil {
		v := *in.CCBill
		out.CCBill = &v
	}
	if in.Stripe != nil {
		v := *in.Stripe
		out.Stripe = &v
	}
	if in.Solana != nil {
		v := *in.Solana
		out.Solana = &v
	}
	if in.CCBill != nil && in.CCBill.AllowedCIDRs != nil {
		out.CCBill.AllowedCIDRs = append([]string(nil), in.CCBill.AllowedCIDRs...)
	}
	if in.Solana != nil && in.Solana.Tokens != nil {
		out.Solana.Tokens = make(map[string]config.TokenConfig, len(in.Solana.Tokens))
		for k, v := range in.Solana.Tokens {
			out.Solana.Tokens[k] = v
		}
	}
	return &out
}
