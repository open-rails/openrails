package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// MerchantDeclaration declares one merchant: an entry of Config.Merchants or
// of a standalone merchant manifest.
type MerchantDeclaration struct {
	// Slug is the merchant's name: slug: in a single-merchant file, the
	// entry's key in Config.Merchants and in a manifest, which refuses the
	// field.
	Slug string `yaml:"-"`
	// Issuer binds the credentials of one issuer to this merchant: every
	// request whose Identity names it as issuer, and whose credential is
	// bound to no scope, acts on this merchant, as its customer or its
	// staff. It is the host's own auth's issuer (AuthKit's Token.Issuer)
	// where the host serves this one merchant with it: an absolute https URL
	// (http on a loopback host), declared by one merchant of an engine at
	// most. Its users are the merchant's own customers. Set it in the host's
	// own code or configuration, per engine; a merchant file never carries
	// it, since several hosts with their own issuers may share one. Empty
	// binds by scope only: a credential must be bound to the merchant's
	// group (authkit/auth Bound), whose id is the merchant's.
	Issuer      string `yaml:"-"`
	DisplayName string `yaml:"display_name"`
	// PSPs is the operator-declared PSP catalog: the merchant's payment
	// service providers, keyed by PSP key (e.g. mobius).
	PSPs PSPs `yaml:"psps,omitempty"`
	// Custodians is the operator-declared custodian catalog, keyed by
	// custodian key. A custodian holds the card; a PSP charges
	// it. Declared once here and referenced by every PSP whose gateway those
	// cards are charged through.
	Custodians Custodians `yaml:"custodians,omitempty"`
	// MerchantSettings are the merchant's settings, beside display_name: the
	// same fields the configuration API reads and applies, validated the same
	// way. Omitted fields keep their stored values; a declared list replaces
	// the stored one.
	billing.MerchantSettings `yaml:",inline"`
	// AlertWebhooks are where the merchant's operational alerts are posted.
	AlertWebhooks []AlertWebhookConfig `yaml:"alert_webhooks,omitempty"`
	// Secrets are the merchant's own credentials.
	Secrets MerchantSecrets `yaml:"secrets,omitempty"`
}

// AlertWebhookConfig is one destination for the merchant's operational
// alerts. Its URL is a credential.
type AlertWebhookConfig struct {
	Name string `yaml:"name,omitempty"`
	URL  string `yaml:"url"`
	// Format is generic (the default), discord or slack.
	Format string `yaml:"format,omitempty"`
	// Enabled defaults to true.
	Enabled *bool `yaml:"enabled,omitempty"`
}

// MerchantSecrets are a merchant's own credentials, kept out of version
// control like a PSP's.
type MerchantSecrets struct {
	// SCIMToken is a provisioning token (at least 32 characters) the
	// merchant's directory presents at /scim/v2. OpenRails keeps its hash as
	// the merchant's declared token, replaced when this changes and removed
	// when it is removed.
	SCIMToken string `yaml:"scim_token,omitempty"`
}

// PSPConfig is one declared PSP: the merchant's account on a rail.
type PSPConfig struct {
	// Rail is the gateway kind: nmi, ccbill, stripe or solana.
	Rail billing.Rail `yaml:"rail"`
	// Environment is the rail's own: nmi live | test | sandbox, stripe live |
	// test, ccbill live | sandbox, solana mainnet | devnet | testnet. Omitted
	// is the rail's real-money one. It is verified with the provider before
	// the PSP is used.
	Environment string `yaml:"environment,omitempty"`
	// AccountID is the card rails' own id for the account (NMI Gateway ID,
	// Stripe acct_…, CCBill clientAccnum-clientSubacc). A Solana PSP has none:
	// its account is its signer's public key.
	AccountID string `yaml:"account_id,omitempty"`
	Archived  bool   `yaml:"archived,omitempty"`
	// Custodian (nmi) references a declared custodian by key:
	// merchants.<slug>.custodians.<key>. "" = this PSP holds its own
	// instruments (Stripe pm_, NMI customer vault), which is the common case
	// and needs no configuration. A key with no matching custodian entry is a
	// hard error, never an unarmed default.
	Custodian string            `yaml:"custodian,omitempty"`
	Secrets   map[string]string `yaml:"secrets,omitempty"`
	// Settings are the rail's other non-secret fields (nmi tokenization_key,
	// stripe publishable_key, …), flat in a PSP document.
	Settings map[string]any `yaml:"settings,omitempty"`
}

// NormalizePSPDeclaration refuses a declared PSP whose fields, keys or
// environment its rail does not take, and returns it in its declared
// environment (the rail's real-money one when omitted).
func NormalizePSPDeclaration(psp string, p PSPConfig) (PSPConfig, error) {
	if err := ValidatePSPKeys(psp, p); err != nil {
		return p, err
	}
	if err := ValidatePSPFields(psp, string(p.Rail), map[string]bool{"custodian": strings.TrimSpace(p.Custodian) != ""}); err != nil {
		return p, err
	}
	if err := ValidatePSPAccount(psp, string(p.Rail), p.AccountID); err != nil {
		return p, err
	}
	environment, err := ResolvePSPEnvironment(psp, string(p.Rail), p.Environment)
	if err != nil {
		return p, err
	}
	p.Environment = environment
	if rpcURL := strings.TrimSpace(p.Secrets[SolanaSecretRPCURL]); p.Rail == billing.Rail(models.RailSolana) && rpcURL != "" {
		if err := ValidateSolanaRPCURL(environment, rpcURL); err != nil {
			return p, fmt.Errorf("psps.%s.%w", psp, err)
		}
	}
	return p, nil
}

// ValidatePSPFields refuses a PSP field its rail does not take: present names
// each field the PSP sets.
func ValidatePSPFields(psp, rail string, present map[string]bool) error {
	rail = strings.ToLower(strings.TrimSpace(rail))
	takes := rails.Fields(models.Rail(rail))
	for _, field := range slices.Sorted(maps.Keys(present)) {
		if present[field] && !slices.Contains(takes, field) {
			return fmt.Errorf("psps.%s.%s: a %s PSP has no %s (%s takes %s)", psp, field, rail, field, rail, strings.Join(PSPDocumentFields(models.Rail(rail)), ", "))
		}
	}
	return nil
}

// ValidatePSPKeys refuses a rail that does not exist and a settings or
// secrets key the rail does not take, naming the PSP and the keys it does: a
// misspelled key would otherwise be stored inert. Secret keys match as the
// secret store reads them (case and surrounding space ignored), settings
// exactly.
func ValidatePSPKeys(psp string, p PSPConfig) error {
	d, ok := rails.Lookup(models.Rail(p.Rail))
	if !ok {
		all := rails.All()
		names := make([]string, 0, len(all))
		for _, r := range all {
			names = append(names, string(r.Rail))
		}
		slices.Sort(names)
		if strings.TrimSpace(string(p.Rail)) == "" {
			return fmt.Errorf("psps.%s.rail is required (%s)", psp, strings.Join(names, ", "))
		}
		return fmt.Errorf("psps.%s.rail: unknown rail %q (%s)", psp, p.Rail, strings.Join(names, ", "))
	}
	var unknown []string
	for _, key := range slices.Sorted(maps.Keys(p.Settings)) {
		if !slices.Contains(d.SettingKeys, key) {
			unknown = append(unknown, strconv.Quote(key))
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("psps.%s: a %s PSP has no %s (%s takes %s)", psp, d.Rail, strings.Join(unknown, ", "), d.Rail, strings.Join(PSPDocumentFields(d.Rail), ", "))
	}
	secrets := make([]string, 0, len(d.CredentialKeys))
	for _, k := range d.CredentialKeys {
		secrets = append(secrets, k.Name)
	}
	unknown = nil
	for _, key := range slices.Sorted(maps.Keys(p.Secrets)) {
		if !slices.Contains(secrets, strings.ToLower(strings.TrimSpace(key))) {
			unknown = append(unknown, strconv.Quote(key))
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	field, takes := "field", "no secrets"
	if len(unknown) > 1 {
		field = "fields"
	}
	if len(secrets) > 0 {
		takes = strings.Join(slices.Sorted(slices.Values(secrets)), ", ")
	}
	return fmt.Errorf("psps.%s.secrets: unknown %s %s (%s takes %s)", psp, field, strings.Join(unknown, ", "), d.Rail, takes)
}

// CustodianConfig is one declared custodian: the merchant's account with a
// card custodian.
type CustodianConfig struct {
	// Kind is the vendor: basis_theory.
	Kind string `yaml:"kind"`
	// Environment is the tenant's money class: live (the default) or test.
	Environment string `yaml:"environment,omitempty"`
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
	// private application key), named in-process
	// custodians/<kind>/<environment>/<account_id>/<key>.
	Secrets map[string]string `yaml:"secrets,omitempty"`
}

// merchantFile is the single-merchant document: the declaration and its slug.
type merchantFile struct {
	Slug                string `yaml:"slug"`
	MerchantDeclaration `yaml:",inline"`
}

// retiredDeclarationKeys are the keys a merchant declaration no longer takes
// and what became of each.
var retiredDeclarationKeys = []struct{ key, fate string }{
	{"rail_merchant_accounts", "was renamed to psps"},
	{"provider_accounts", "was renamed to psps"},
	{"api_host", "was removed: a request names its merchant by its credential, never by its host"},
}

// RetiredDeclarationKey is the first retired key doc sets, with what became
// of it. The strict parser would refuse it as an unknown field, which reads
// like a typo.
func RetiredDeclarationKey(doc map[string]any) (string, bool) {
	for _, retired := range retiredDeclarationKeys {
		if _, ok := doc[retired.key]; ok {
			return retired.key + " " + retired.fate, true
		}
	}
	return "", false
}

// ParseMerchantDeclaration parses one merchant YAML document, which names its
// merchant with a required slug. Unknown fields are refused, so a typo fails
// loudly instead of declaring a merchant with no PSPs.
func ParseMerchantDeclaration(raw []byte) (MerchantDeclaration, error) {
	var probe map[string]any
	if yaml.Unmarshal(raw, &probe) == nil {
		if retired, ok := RetiredDeclarationKey(probe); ok {
			return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: %s", retired)
		}
		if _, ok := probe["issuer"]; ok {
			return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: issuer is not a merchant file's: each host sets MerchantDeclaration.Issuer in its own configuration")
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
	for _, key := range slices.Sorted(maps.Keys(m.PSPs)) {
		p, err := NormalizePSPDeclaration(key, m.PSPs[key])
		if err != nil {
			return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: %w", err)
		}
		m.PSPs[key] = p
	}
	m.Slug = billing.NormalizeMerchantSlug(f.Slug)
	return m, nil
}

// ConfigurationFields names what m sets beyond its merchant's slug: what
// Vault holds when it is the source.
func ConfigurationFields(m MerchantDeclaration) []string {
	var out []string
	if strings.TrimSpace(m.DisplayName) != "" {
		out = append(out, "display_name")
	}
	if len(m.PSPs) > 0 {
		out = append(out, "psps")
	}
	if len(m.Custodians) > 0 {
		out = append(out, "custodians")
	}
	settings := reflect.ValueOf(m.MerchantSettings)
	for i := range settings.NumField() {
		if !settings.Field(i).IsZero() {
			name, _, _ := strings.Cut(settings.Type().Field(i).Tag.Get("json"), ",")
			out = append(out, name)
		}
	}
	if len(m.AlertWebhooks) > 0 {
		out = append(out, "alert_webhooks")
	}
	if strings.TrimSpace(m.Secrets.SCIMToken) != "" {
		out = append(out, "secrets")
	}
	return out
}

// ErrDeclarationBesideVault refuses a file declaring merchant configuration
// while Vault holds it.
var ErrDeclarationBesideVault = errors.New("Vault holds merchant configuration (vault.kv_mount); a merchant declaration names only its slug")

// RefuseDeclarationBesideVault refuses m when Vault is the source and m
// declares configuration, naming its fields.
func RefuseDeclarationBesideVault(cfg *Config, slug string, m MerchantDeclaration) error {
	if MerchantConfigKVMount(cfg) == "" {
		return nil
	}
	if fields := ConfigurationFields(m); len(fields) > 0 {
		return fmt.Errorf("merchant %q declares %s: %w", slug, strings.Join(fields, ", "), ErrDeclarationBesideVault)
	}
	return nil
}

// DeclaredMerchants is cfg.Merchants in slug order, each named by its key,
// after the checks that need no database. One entry's mistake refuses that
// entry alone, reported in refused: a key that is not a slug, or a Slug that
// is not its key, refuses the merchant; an Issuer that is not an absolute
// https URL (http on a loopback host), or that another entry declares too, is
// bound to no merchant. Each PSP is checked as its merchant is provisioned.
func DeclaredMerchants(cfg *Config) (out []MerchantDeclaration, refused []Degradation, err error) {
	byIssuer := map[string][]string{}
	for _, key := range slices.Sorted(maps.Keys(cfg.Merchants)) {
		m := cfg.Merchants[key]
		slug := billing.NormalizeMerchantSlug(key)
		if err := billing.ValidateMerchantSlug(key); err != nil {
			refused = append(refused, Degradation{Field: "merchants." + key, Err: fmt.Errorf("%w; the merchant is not provisioned", err)})
			continue
		}
		if m.Slug != "" && billing.NormalizeMerchantSlug(m.Slug) != slug {
			refused = append(refused, Degradation{Field: "merchants." + key, Err: fmt.Errorf("Slug %q is not its key; the merchant is not provisioned", m.Slug)})
			continue
		}
		m.Slug = slug
		if m.Issuer = strings.TrimSpace(m.Issuer); m.Issuer != "" {
			if err := validateIssuer(m.Issuer); err != nil {
				refused = append(refused, Degradation{Field: "merchants." + key + ".issuer", Err: fmt.Errorf("%w; no credential is bound to the merchant by issuer", err)})
				m.Issuer = ""
			} else {
				byIssuer[m.Issuer] = append(byIssuer[m.Issuer], slug)
			}
		}
		// A PSP's omitted environment is its rail's real-money one. One that
		// does not check is refused alone as its merchant is provisioned.
		psps := make(map[string]PSPConfig, len(m.PSPs))
		for _, psp := range slices.Sorted(maps.Keys(m.PSPs)) {
			p := m.PSPs[psp]
			if normalized, err := NormalizePSPDeclaration(psp, p); err == nil {
				p = normalized
			}
			psps[psp] = p
		}
		if m.PSPs != nil {
			m.PSPs = psps
		}
		out = append(out, m)
	}
	for i, m := range out {
		if others := byIssuer[m.Issuer]; len(others) > 1 {
			refused = append(refused, Degradation{Field: "merchants." + m.Slug + ".issuer", Err: fmt.Errorf("%q is declared by %s: an issuer binds its credentials to one merchant, so it binds none", m.Issuer, strings.Join(others, ", "))})
			out[i].Issuer = ""
		}
	}
	return out, refused, nil
}

// validateIssuer accepts an issuer identifier as tokens carry it (RFC 8414
// §2): an absolute https URL without query or fragment, or http on a
// loopback host.
func validateIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(issuer, "#") {
		return fmt.Errorf("%q is not an absolute URL without query or fragment", issuer)
	}
	host := u.Hostname()
	loopback := host == "localhost" || net.ParseIP(host).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return fmt.Errorf("%q must be https (http only on a loopback host)", issuer)
	}
	return nil
}
