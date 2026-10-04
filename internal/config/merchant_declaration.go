package config

import (
	"fmt"

	"github.com/goccy/go-yaml"

	"github.com/open-rails/openrails/billing"
)

// MerchantDeclaration declares one merchant: the embedded engine's merchant
// (Config.Merchant) or one entry of a standalone merchant manifest.
type MerchantDeclaration struct {
	// Slug is the merchant's name. A manifest keys merchants by slug instead.
	Slug        string `yaml:"-"`
	DisplayName string `yaml:"display_name"`
	// APIHost is the merchant's canonical API host (e.g. "api.myapp.example"):
	// the Host-header value public routes resolve this merchant from.
	// Globally unique across active merchants. Omitted leaves the stored value
	// untouched (it can also be assigned via PUT /v1/merchant/api-host).
	APIHost string `yaml:"api_host,omitempty"`
	// PSPs is the operator-declared PSP catalog: the merchant's payment
	// service providers, keyed by PSP key (e.g. mobius), one rail each.
	PSPs map[string]PSPConfig `yaml:"psps,omitempty"`
	// Custodians is the operator-declared custodian catalog, keyed by
	// custodian key, one kind each. A custodian holds the card; a PSP charges
	// it. Declared once here and referenced by every PSP whose gateway those
	// cards are charged through.
	Custodians map[string]CustodianConfig `yaml:"custodians,omitempty"`
	// Settings are the merchant's settings: the same document the
	// configuration API reads and applies, validated the same way. Omitted
	// fields keep their stored values; a declared list replaces the stored one.
	Settings billing.MerchantSettings `yaml:"settings,omitempty"`
}

// PSPConfig is one declared PSP: exactly one entry, keyed by its rail (nmi,
// ccbill, stripe, solana).
type PSPConfig map[string]ProviderRailAccountConfig

// CustodianConfig is one declared custodian's kind block: exactly one entry,
// keyed by the vendor kind (basis_theory), mirroring PSPConfig's rail key.
type CustodianConfig map[string]CustodianAccountConfig

// CustodianAccountConfig is one merchant-owned account with a custodian.
type CustodianAccountConfig struct {
	// AccountID is the custodian-native tenant identity (Basis Theory: the
	// tenant id). Operator-declared: there is no runtime lookup.
	AccountID string `yaml:"account_id,omitempty"`
	// Archived drains the custodian: instruments it holds stay chargeable, no
	// new arrangement may reference it.
	Archived bool `yaml:"archived,omitempty"`
	// Settings are the declared NON-secret knobs, validated against the kind's
	// registry (internal/custodians): public_api_key, network_tokens.
	Settings map[string]any `yaml:"settings,omitempty"`
	// Secrets are the kind's credential slots (Basis Theory: api_key, the
	// private application key). Stored under
	// custodians/<kind>/<environment>/<account_id>/<key>.
	Secrets map[string]string `yaml:"secrets,omitempty"`
}

// ProviderRailAccountConfig is a PSP's account on its rail.
type ProviderRailAccountConfig struct {
	// LegacyEnvironment keeps the retired `environment:` key parseable ONLY so a
	// manifest that still declares it fails loudly. The environment is
	// DERIVED from test_mode — it never was anything else, since a declared value
	// that disagreed refused to boot and one that agreed was a no-op.
	LegacyEnvironment string `yaml:"environment,omitempty"`
	AccountID         string `yaml:"account_id,omitempty"`
	Archived          bool   `yaml:"archived,omitempty"`
	// Custodian references a declared custodian by key:
	// merchants.<slug>.custodians.<key>. "" = this PSP holds its own
	// instruments (Stripe pm_, NMI customer vault), which is the common case
	// and needs no configuration. A key with no matching custodian entry is a
	// hard error, never an unarmed default.
	Custodian string            `yaml:"custodian,omitempty"`
	Signer    *PSPSignerConfig  `yaml:"signer,omitempty"`
	Secrets   map[string]string `yaml:"secrets,omitempty"`
	// Settings are per-account NON-SECRET runtime knobs, stored on the
	// psps row (NMI: tokenization_key/tokenization_url;
	// Solana: rpc_provider, rpc_api_key, tokens,
	// recipient_wallet).
	Settings map[string]any `yaml:"settings,omitempty"`
}

// PSPSignerConfig selects how a Solana PSP signs: Mode is "local_keypair" or
// "vault_transit", and Key names the Vault Transit key.
type PSPSignerConfig struct {
	Mode string `yaml:"mode,omitempty"`
	Key  string `yaml:"key,omitempty"`
}

// ParseMerchantDeclaration parses one merchant YAML document. Unknown fields
// are refused, so a typo fails loudly instead of declaring a merchant with no
// PSPs; the slug comes from the caller, not the document.
func ParseMerchantDeclaration(raw []byte) (MerchantDeclaration, error) {
	var probe map[string]any
	if yaml.Unmarshal(raw, &probe) == nil {
		for _, old := range []string{"rail_merchant_accounts", "provider_accounts"} {
			if _, ok := probe[old]; ok {
				return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: %s was renamed to psps", old)
			}
		}
	}
	var m MerchantDeclaration
	if err := yaml.UnmarshalWithOptions(raw, &m, yaml.DisallowUnknownField()); err != nil {
		return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: %w", err)
	}
	return m, nil
}
