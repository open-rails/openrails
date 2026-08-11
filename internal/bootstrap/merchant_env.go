package bootstrap

import (
	"fmt"
	"os"
	"strings"

	"github.com/open-rails/openrails/internal/custodians"
)

const MerchantBillingEnvPrefix = "BILLING_"

// MerchantBillingEnvKey maps a BILLING_ env var to a koanf key for BillingConfig.
//
// or#915: the env overlay carries ONLY credentials + branding. Secrets are
// what the overlay exists for (they must stay out of committed YAML);
// display_name/profile ride along as harmless branding. Everything else —
// invoice policy, api_host, wasted-spend windows, PSP/custodian account
// state (account_id, settings, archived, custodian, signer) — lives in the
// manifest YAML or its DB/API home, and a BILLING_MERCHANTS_* var naming one
// refuses boot with that home in the message (rejectRenamedMerchantEnvName).
//
// Routable shapes (schema-aware so single underscores need no array indexes):
//
//	BILLING_VERSION -> version
//	BILLING_MERCHANTS_DOUJINS_DISPLAY_NAME -> merchants.doujins.display_name
//	BILLING_MERCHANTS_DOUJINS_PROFILE_FROM_EMAIL -> merchants.doujins.profile.from_email
//	BILLING_MERCHANTS_DOUJINS_PSPS_MOBIUS_NMI_SECRETS_SECURITY_KEY
//	-> merchants.doujins.psps.mobius.nmi.secrets.security_key
//	BILLING_MERCHANTS_DOUJINS_CUSTODIANS_BT_BASIS_THEORY_SECRETS_API_KEY
//	-> merchants.doujins.custodians.bt.basis_theory.secrets.api_key
//
// Map-key spans are lower-kebab-cased. Keep merchant/PSP keys
// lowercase, avoid underscores in the YAML keys, and avoid the fixed section
// words (PSPS, PROFILE, ...) inside merchant/account key spans.
func MerchantBillingEnvKey(envName string) string {
	s := strings.ToUpper(strings.TrimSpace(envName))
	s = strings.TrimPrefix(s, MerchantBillingEnvPrefix)
	if s == "" {
		return ""
	}
	tokens := strings.Split(s, "_")
	if len(tokens) == 1 && tokens[0] == "VERSION" {
		return "version"
	}
	if len(tokens) < 3 || tokens[0] != "MERCHANTS" {
		return ""
	}
	section, sectionIdx, sectionWidth := firstMerchantSection(tokens[1:])
	if sectionIdx < 0 || sectionIdx == 0 {
		return ""
	}
	merchantKey := envKeySpan(tokens[1 : 1+sectionIdx])
	rest := tokens[1+sectionIdx+sectionWidth:]
	base := "merchants." + merchantKey + "." + section
	switch section {
	case "display_name":
		if len(rest) == 0 {
			return base
		}
	case "profile":
		if len(rest) > 0 {
			return base + "." + strings.ToLower(strings.Join(rest, "_"))
		}
	case "psps":
		return pspEnvKey(base, rest)
	case "custodians":
		return custodianEnvKey(base, rest)
	}
	return ""
}

// firstMerchantSection recognizes every CURRENT AND RETIRED section word so a
// var targeting a section or#915 removed from the env-routable set still
// token-splits correctly and gets its section-specific refusal, instead of the
// generic "does not route".
func firstMerchantSection(tokens []string) (string, int, int) {
	sections := []struct {
		raw string
		key string
	}{
		{"DISPLAY_NAME", "display_name"},
		{"API_HOST", "api_host"},
		{"PROFILE", "profile"},
		{"INVOICE", "invoice"},
		{"PSPS", "psps"},
		{"CUSTODIANS", "custodians"},
		{"DELEGATED_INVOKER_WASTED_SPEND_WINDOWS", "delegated_invoker_wasted_spend_windows"},
	}
	for i := range tokens {
		for _, section := range sections {
			parts := strings.Split(section.raw, "_")
			if hasPrefixTokens(tokens[i:], parts) {
				return section.key, i, len(parts)
			}
		}
	}
	return "", -1, 0
}

// pspEnvKey routes ONLY the secrets branch (or#915): account_id, settings,
// archived, custodian and signer are manifest/DB state, not env overlay.
func pspEnvKey(base string, tokens []string) string {
	railIdx := -1
	for i, token := range tokens {
		switch token {
		case "NMI", "STRIPE", "CCBILL", "SOLANA":
			railIdx = i
		}
	}
	if railIdx <= 0 || railIdx == len(tokens)-1 {
		return ""
	}
	rest := tokens[railIdx+1:]
	if rest[0] != "SECRETS" || len(rest) < 2 {
		return ""
	}
	accountKey := envKeySpan(tokens[:railIdx])
	rail := strings.ToLower(tokens[railIdx])
	return base + "." + accountKey + "." + rail + ".secrets." + strings.ToLower(strings.Join(rest[1:], "_"))
}

// custodianEnvKey is pspEnvKey's custody sibling (or#880), secrets-only like
// it (or#915). The kind tokens come from the custodian registry, so a new
// vendor is routable through env overlays the moment its descriptor exists —
// no second list.
func custodianEnvKey(base string, tokens []string) string {
	kindIdx, kindWidth, kind := -1, 0, ""
	for _, declared := range custodians.Kinds() {
		parts := strings.Split(strings.ToUpper(declared), "_")
		for i := range tokens {
			if hasPrefixTokens(tokens[i:], parts) {
				kindIdx, kindWidth, kind = i, len(parts), declared
			}
		}
	}
	if kindIdx <= 0 || kindIdx+kindWidth >= len(tokens) {
		return ""
	}
	rest := tokens[kindIdx+kindWidth:]
	if rest[0] != "SECRETS" || len(rest) < 2 {
		return ""
	}
	return base + "." + envKeySpan(tokens[:kindIdx]) + "." + kind + ".secrets." + strings.ToLower(strings.Join(rest[1:], "_"))
}

// rejectRenamedMerchantEnvVars fails loudly when a BILLING_MERCHANTS_* env var
// still uses a retired PSP anchor (PSPS <- ACCOUNTS <- RAIL_MERCHANT_ACCOUNTS
// <- PROVIDER_ACCOUNTS). Without this the old var
// would not just be dropped — the single-token PSPS anchor would mis-split
// it into a wrong merchant key ("doujins-rail-merchant") and overlay config
// nobody declared. The retired anchors are therefore poison token sequences
// anywhere in the name (which also means a merchant key span must not contain
// them, e.g. a merchant slugged "provider" cannot use env overlays).
//
// It also rejects any BILLING_MERCHANTS_* var that routes to no manifest field
// (#710): the namespace is ours, so a typo'd or retired name (e.g. the removed
// ISSUER section) is an error, never a silent drop.
func rejectRenamedMerchantEnvVars() error {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if err := rejectRenamedMerchantEnvName("env", name); err != nil {
			return err
		}
	}
	return nil
}

// rejectRenamedMerchantEnvName applies the poison-token and no-route checks to
// a single BILLING_MERCHANTS_* name; source labels the origin (env / secret
// file) in the error.
func rejectRenamedMerchantEnvName(source, name string) error {
	if !strings.HasPrefix(name, MerchantBillingEnvPrefix+"MERCHANTS_") {
		return nil
	}
	tokens := strings.Split(name, "_")
	for _, old := range [][]string{
		{"RAIL", "MERCHANT", "ACCOUNTS"},
		{"PROVIDER", "ACCOUNTS"},
		{"ACCOUNTS"},
	} {
		for i := range tokens {
			if hasPrefixTokens(tokens[i:], old) {
				return fmt.Errorf("%s %s: %s was renamed to PSPS (merchants.<slug>.psps); use BILLING_MERCHANTS_<MERCHANT>_PSPS_...", source, name, strings.Join(old, "_"))
			}
		}
	}
	if MerchantBillingEnvKey(name) == "" {
		if hint := removedMerchantEnvSectionHint(name); hint != "" {
			return fmt.Errorf("%s %s: %s", source, name, hint)
		}
		return fmt.Errorf("%s %s does not route to a merchant manifest field (#710); env overlays carry only credentials + branding (or#915: secrets, display_name, profile) — see MerchantBillingEnvKey for the accepted BILLING_MERCHANTS_* shapes", source, name)
	}
	return nil
}

// removedMerchantEnvSectionHint names the manifest/DB home of every merchant
// section or#915 removed from the env-routable set, so an operator booting
// with an old var learns immediately where the value lives now.
func removedMerchantEnvSectionHint(name string) string {
	s := strings.ToUpper(strings.TrimSpace(name))
	s = strings.TrimPrefix(s, MerchantBillingEnvPrefix)
	tokens := strings.Split(s, "_")
	if len(tokens) < 3 || tokens[0] != "MERCHANTS" {
		return ""
	}
	section, sectionIdx, sectionWidth := firstMerchantSection(tokens[1:])
	if sectionIdx <= 0 {
		return ""
	}
	rest := tokens[1+sectionIdx+sectionWidth:]
	hasSecrets := false
	for _, token := range rest {
		if token == "SECRETS" {
			hasSecrets = true
		}
	}
	switch section {
	case "invoice":
		return "invoice policy left the env overlay (or#915: env carries only credentials + branding) — declare merchants.<slug>.invoice in the manifest YAML (openrails push-merchant-config; stored on merchant_configurations)"
	case "api_host":
		return "api_host left the env overlay (or#915: env carries only credentials + branding) — declare merchants.<slug>.api_host in the manifest YAML or assign it via PUT /v1/merchant/api-host"
	case "delegated_invoker_wasted_spend_windows":
		return "delegated_invoker_wasted_spend_windows left the env overlay (or#915: env carries only credentials + branding) — declare merchants.<slug>.delegated_invoker_wasted_spend_windows in the manifest YAML (openrails push-merchant-config)"
	case "psps":
		if !hasSecrets {
			return "PSP account state (account_id, settings, archived, custodian, signer) left the env overlay (or#915: env carries only credentials + branding) — only ..._PSPS_<KEY>_<RAIL>_SECRETS_* remains env-routable; declare the field under merchants.<slug>.psps in the manifest YAML (stored on the openrails.psps row)"
		}
	case "custodians":
		if !hasSecrets {
			return "custodian account state (account_id, settings, archived) left the env overlay (or#915: env carries only credentials + branding) — only ..._CUSTODIANS_<KEY>_<KIND>_SECRETS_* remains env-routable; declare the field under merchants.<slug>.custodians in the manifest YAML"
		}
	}
	return ""
}

func envKeySpan(tokens []string) string {
	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		token = strings.ToLower(strings.TrimSpace(token))
		if token != "" {
			parts = append(parts, token)
		}
	}
	return strings.Join(parts, "-")
}

func hasPrefixTokens(tokens, prefix []string) bool {
	if len(tokens) < len(prefix) {
		return false
	}
	for i := range prefix {
		if tokens[i] != prefix[i] {
			return false
		}
	}
	return true
}
