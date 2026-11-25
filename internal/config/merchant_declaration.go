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

	"github.com/open-rails/openrails/internal/disputerules"

	"github.com/goccy/go-yaml"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// NormalizePSPDeclaration refuses a declared PSP whose fields, keys or
// environment its rail does not take, and returns it in its declared
// environment (the rail's real-money one when omitted).
func NormalizePSPDeclaration(psp string, p billing.PSPConfig) (billing.PSPConfig, error) {
	if err := ValidatePSPKeys(psp, p); err != nil {
		return p, err
	}
	if err := ValidatePSPFields(psp, string(p.Rail), map[string]bool{"custodian": strings.TrimSpace(p.Custodian) != ""}); err != nil {
		return p, err
	}
	if err := ValidatePSPAccount(psp, string(p.Rail), p.AccountID); err != nil {
		return p, err
	}
	environment, err := ResolvePSPEnvironment(psp, string(p.Rail), string(p.Environment))
	if err != nil {
		return p, err
	}
	p.Environment = billing.Environment(environment)
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
func ValidatePSPKeys(psp string, p billing.PSPConfig) error {
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
	if limit, ok := p.Settings["dispute_ratio_limit"]; ok {
		if _, err := disputerules.ParseRatioLimit(limit); err != nil {
			return fmt.Errorf("psps.%s: %w", psp, err)
		}
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

// ParseMerchantDocument parses one merchant YAML document, which names its
// merchant with a required slug. Unknown fields are refused, so a typo fails
// loudly instead of declaring a merchant with no PSPs.
func ParseMerchantDocument(raw []byte) (billing.MerchantDocument, error) {
	var probe map[string]any
	if yaml.Unmarshal(raw, &probe) == nil {
		if _, ok := probe["issuer"]; ok {
			return billing.MerchantDocument{}, fmt.Errorf("parse merchant document: issuer is not a document's: each host sets MerchantDeclaration.Issuer in its own code")
		}
	}
	var m billing.MerchantDocument
	if err := yaml.UnmarshalWithOptions(raw, &m, yaml.DisallowUnknownField()); err != nil {
		return billing.MerchantDocument{}, fmt.Errorf("parse merchant document: %w", err)
	}
	if strings.TrimSpace(m.Slug) == "" {
		return billing.MerchantDocument{}, fmt.Errorf("parse merchant document: slug is required")
	}
	if err := billing.ValidateMerchantSlug(m.Slug); err != nil {
		return billing.MerchantDocument{}, fmt.Errorf("parse merchant document: %w", err)
	}
	for _, key := range slices.Sorted(maps.Keys(m.PSPs)) {
		p, err := NormalizePSPDeclaration(key, m.PSPs[key])
		if err != nil {
			return billing.MerchantDocument{}, fmt.Errorf("parse merchant document: %w", err)
		}
		m.PSPs[key] = p
	}
	m.Slug = billing.NormalizeMerchantSlug(m.Slug)
	return m, nil
}

// ConfigurationFields names what m sets beyond its merchant's slug: what
// Vault holds when it is the source.
func ConfigurationFields(m billing.MerchantDocument) []string {
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
func RefuseDeclarationBesideVault(cfg *billing.Config, slug string, m billing.MerchantDocument) error {
	if MerchantConfigKVMount(cfg) == "" {
		return nil
	}
	if fields := ConfigurationFields(m); len(fields) > 0 {
		return fmt.Errorf("merchant %q declares %s: %w", slug, strings.Join(fields, ", "), ErrDeclarationBesideVault)
	}
	return nil
}

// DeclaredMerchants is cfg.Merchants in slug order, after the checks that
// need no database. One entry's mistake refuses that entry alone, reported in
// refused: a slug that is not one refuses the merchant, and a slug declared
// twice refuses every entry declaring it; an Issuer that is not an absolute
// https URL (http on a loopback host), or that another entry declares too, is
// bound to no merchant. Each PSP is checked as its merchant is provisioned.
func DeclaredMerchants(cfg *billing.Config) (out []billing.MerchantDeclaration, refused []Degradation) {
	bySlug := map[string][]int{}
	for i, m := range cfg.Merchants {
		if err := billing.ValidateMerchantSlug(m.Slug); err != nil {
			refused = append(refused, Degradation{Field: fmt.Sprintf("merchants[%d]", i), Err: fmt.Errorf("%w; the merchant is not provisioned", err)})
			continue
		}
		slug := billing.NormalizeMerchantSlug(m.Slug)
		bySlug[slug] = append(bySlug[slug], i)
	}
	byIssuer := map[string][]string{}
	for _, slug := range slices.Sorted(maps.Keys(bySlug)) {
		if entries := bySlug[slug]; len(entries) > 1 {
			names := make([]string, len(entries))
			for k, i := range entries {
				names[k] = strconv.Itoa(i)
			}
			refused = append(refused, Degradation{Field: "merchants." + slug, Err: fmt.Errorf("declared by merchants[%s]: a slug names one merchant, so none is provisioned", strings.Join(names, "], merchants["))})
			continue
		}
		m := cfg.Merchants[bySlug[slug][0]]
		if m.Binding != nil {
			binding := *m.Binding
			m.Binding = &binding
		}
		m.Slug = slug
		if m.Issuer = strings.TrimSpace(m.Issuer); m.Issuer != "" {
			if err := validateIssuer(m.Issuer); err != nil {
				refused = append(refused, Degradation{Field: "merchants." + slug + ".issuer", Err: fmt.Errorf("%w; no credential is bound to the merchant by issuer", err)})
				m.Issuer = ""
			} else {
				byIssuer[m.Issuer] = append(byIssuer[m.Issuer], slug)
			}
		}
		// A PSP's omitted environment is its rail's real-money one. One that
		// does not check is refused alone as its merchant is provisioned.
		if m.PSPs != nil {
			psps := make(map[string]billing.PSPConfig, len(m.PSPs))
			for _, psp := range slices.Sorted(maps.Keys(m.PSPs)) {
				p := m.PSPs[psp]
				if normalized, err := NormalizePSPDeclaration(psp, p); err == nil {
					p = normalized
				}
				psps[psp] = p
			}
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
	return out, refused
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
