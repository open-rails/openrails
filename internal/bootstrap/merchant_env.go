package bootstrap

import (
	"fmt"
	"os"
	"strings"
)

const MerchantBillingEnvPrefix = "BILLING_"

// MerchantBillingEnvKey maps a BILLING_ env var to a koanf key for BillingConfig.
// It is schema-aware so single underscores can be used without array indexes:
//
//	BILLING_MERCHANTS_DOUJINS_ACCOUNTS_CCBILL_CCBILL_SECRETS_DATALINK_USERNAME
//	-> merchants.doujins.accounts.ccbill.ccbill.secrets.datalink_username
//	BILLING_MERCHANTS_DOUJINS_ACCOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY
//	-> merchants.doujins.accounts.mobius.nmi.secrets.security_key
//
// Map-key spans are lower-kebab-cased. Keep merchant/provider account keys
// lowercase, avoid underscores in the YAML keys, and avoid the fixed section
// words (ACCOUNTS, PROFILE, ...) inside merchant/account key spans.
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
	case "issuer", "profile", "invoice":
		if len(rest) > 0 {
			return base + "." + strings.ToLower(strings.Join(rest, "_"))
		}
	case "accounts":
		return providerAccountEnvKey(base, rest)
	}
	return ""
}

func firstMerchantSection(tokens []string) (string, int, int) {
	sections := []struct {
		raw string
		key string
	}{
		{"DISPLAY_NAME", "display_name"},
		{"ISSUER", "issuer"},
		{"PROFILE", "profile"},
		{"INVOICE", "invoice"},
		{"ACCOUNTS", "accounts"},
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

func providerAccountEnvKey(base string, tokens []string) string {
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
	accountKey := envKeySpan(tokens[:railIdx])
	rail := strings.ToLower(tokens[railIdx])
	rest := tokens[railIdx+1:]
	accountBase := base + "." + accountKey + "." + rail
	if len(rest) == 1 {
		return accountBase + "." + strings.ToLower(rest[0])
	}
	switch rest[0] {
	case "SECRETS":
		if len(rest) > 1 {
			return accountBase + ".secrets." + strings.ToLower(strings.Join(rest[1:], "_"))
		}
	case "SETTINGS":
		if len(rest) > 1 {
			return accountBase + ".settings." + strings.ToLower(strings.Join(rest[1:], "_"))
		}
	case "SIGNER":
		if len(rest) > 1 {
			return accountBase + ".signer." + strings.ToLower(strings.Join(rest[1:], "_"))
		}
	default:
		return accountBase + "." + strings.ToLower(strings.Join(rest, "_"))
	}
	return ""
}

// rejectRenamedMerchantEnvVars fails loudly when a BILLING_MERCHANTS_* env var
// still uses a retired accounts anchor (#698: ACCOUNTS <- RAIL_MERCHANT_ACCOUNTS;
// #683: RAIL_MERCHANT_ACCOUNTS <- PROVIDER_ACCOUNTS). Without this the old var
// would not just be dropped — the single-token ACCOUNTS anchor would mis-split
// it into a wrong merchant key ("doujins-rail-merchant") and overlay config
// nobody declared. The retired anchors are therefore poison token sequences
// anywhere in the name (which also means a merchant key span must not contain
// them, e.g. a merchant slugged "provider" cannot use env overlays).
func rejectRenamedMerchantEnvVars() error {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, MerchantBillingEnvPrefix+"MERCHANTS_") {
			continue
		}
		tokens := strings.Split(name, "_")
		for _, old := range [][]string{
			{"RAIL", "MERCHANT", "ACCOUNTS"},
			{"PROVIDER", "ACCOUNTS"},
		} {
			for i := range tokens {
				if hasPrefixTokens(tokens[i:], old) {
					return fmt.Errorf("env %s: %s was renamed to ACCOUNTS (merchants.<slug>.accounts, #698); use BILLING_MERCHANTS_<MERCHANT>_ACCOUNTS_...", name, strings.Join(old, "_"))
				}
			}
		}
	}
	return nil
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
