package config

import (
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/open-rails/openrails/billing"
)

// MerchantDeclaration declares one merchant: the embedded engine's merchant
// (Config.Merchant) or one entry of a standalone merchant manifest.
type MerchantDeclaration struct {
	// Slug is the merchant's name: slug: in a single-merchant file, the
	// entry's key in a manifest, which refuses the field.
	Slug        string `yaml:"-"`
	DisplayName string `yaml:"display_name"`
	// APIHost is the merchant's canonical API host (e.g. "api.myapp.example"):
	// the Host-header value public routes resolve this merchant from.
	// Globally unique across active merchants. Omitted leaves the stored value
	// untouched (it can also be assigned via PUT /v1/merchant/api-host).
	APIHost string `yaml:"api_host,omitempty"`
	// PSPs is the operator-declared PSP catalog: the merchant's payment
	// service providers, keyed by PSP key (e.g. mobius).
	PSPs map[string]PSPConfig `yaml:"psps,omitempty"`
	// Custodians is the operator-declared custodian catalog, keyed by
	// custodian key. A custodian holds the card; a PSP charges
	// it. Declared once here and referenced by every PSP whose gateway those
	// cards are charged through.
	Custodians map[string]CustodianConfig `yaml:"custodians,omitempty"`
	// Settings are the merchant's settings: the same document the
	// configuration API reads and applies, validated the same way. Omitted
	// fields keep their stored values; a declared list replaces the stored one.
	Settings billing.MerchantSettings `yaml:"settings,omitempty"`
}

// PSPConfig is one declared PSP: the merchant's account on a rail.
type PSPConfig struct {
	// Rail is the gateway kind: nmi, ccbill, stripe or solana.
	Rail billing.Rail `yaml:"rail"`
	// AccountID is the rail's own id for the account (NMI Gateway ID, Stripe
	// acct_…, CCBill clientAccnum-clientSubacc). Solana derives it from the
	// signer.
	AccountID string `yaml:"account_id,omitempty"`
	Archived  bool   `yaml:"archived,omitempty"`
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

// CustodianConfig is one declared custodian: the merchant's account with a
// card custodian.
type CustodianConfig struct {
	// Kind is the vendor: basis_theory.
	Kind string `yaml:"kind"`
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

// PSPSignerConfig selects how a Solana PSP signs: Mode is "local_keypair" or
// "vault_transit", and Key names the Vault Transit key.
type PSPSignerConfig struct {
	Mode string `yaml:"mode,omitempty"`
	Key  string `yaml:"key,omitempty"`
}

// merchantFile is the single-merchant document: the declaration and its slug.
type merchantFile struct {
	Slug                string `yaml:"slug"`
	MerchantDeclaration `yaml:",inline"`
}

// ParseMerchantDeclaration parses one merchant YAML document, which names its
// merchant with a required slug. Unknown fields are refused, so a typo fails
// loudly instead of declaring a merchant with no PSPs.
func ParseMerchantDeclaration(raw []byte) (MerchantDeclaration, error) {
	var probe map[string]any
	if yaml.Unmarshal(raw, &probe) == nil {
		for _, old := range []string{"rail_merchant_accounts", "provider_accounts"} {
			if _, ok := probe[old]; ok {
				return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: %s was renamed to psps", old)
			}
		}
	}
	var f merchantFile
	if err := yaml.UnmarshalWithOptions(raw, &f, yaml.DisallowUnknownField()); err != nil {
		return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: %w", err)
	}
	if strings.TrimSpace(f.Slug) == "" {
		return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: slug is required")
	}
	if err := billing.ValidateMerchantSlug(f.Slug); err != nil {
		return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: %w", err)
	}
	m := f.MerchantDeclaration
	m.Slug = billing.NormalizeMerchantSlug(f.Slug)
	return m, nil
}
