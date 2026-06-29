package embedded

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
)

// PaymentProvider is one provider-account credential set supplied by an
// embedding host. Name is an optional local selector only; OpenRails durable
// identity comes from the provider account id resolved at runtime.
type PaymentProvider struct {
	// Name is an optional local config selector such as "stripe_primary" or
	// "mobius". Leave it empty when the host does not need to target the
	// account by name; OpenRails will generate a process-local selector.
	Name string
	// Config is the provider-account credential/config payload. Set Config.Rail
	// for non-reserved names and for generated names. Config.Routing defaults to default.
	Config config.ProviderAccountConfig
}

// ApplyPaymentProviders converts host-supplied embedded provider credentials
// into an in-memory ProviderAccountSet. Local names are not durable identity;
// provider account rows store the provider-returned account id.
func ApplyPaymentProviders(providers []PaymentProvider) (config.ProviderAccountSet, error) {
	set := config.ProviderAccountSet{}
	if len(providers) == 0 {
		return set, nil
	}
	seen := map[string]struct{}{}
	for _, provider := range providers {
		proc := provider.Config
		key := strings.ToLower(strings.TrimSpace(provider.Name))
		rail := proc.EffectiveRail(key)
		if rail == "" {
			return nil, fmt.Errorf("embedded payment provider %q requires config rail", key)
		}
		if proc.Rail == "" {
			proc.Rail = rail
		}
		if key == "" {
			key = generatedProviderName(set, seen, rail)
		}
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate embedded payment provider name %q", key)
		}
		seen[key] = struct{}{}
		set[key] = cloneRailConfig(&proc)
	}
	return set, nil
}

func generatedProviderName(existing map[string]*config.ProviderAccountConfig, seen map[string]struct{}, rail models.Rail) string {
	base := strings.ToLower(strings.TrimSpace(string(rail)))
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

func cloneRailConfig(in *config.ProviderAccountConfig) *config.ProviderAccountConfig {
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
