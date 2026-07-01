package bootstrap

import "strings"

const MerchantBillingEnvPrefix = "BILLING_"

// MerchantBillingEnvKey maps a BILLING_ env var to a koanf key for BillingConfig.
// It is schema-aware so single underscores can be used without array indexes:
//
//	BILLING_MERCHANTS_DOUJINS_PROVIDER_ACCOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY
//	-> merchants.doujins.provider_accounts.mobius.nmi.secrets.security_key
//
// Map-key spans are lower-kebab-cased. Keep merchant/provider account keys
// lowercase and avoid underscores in the YAML keys.
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
	case "provider_accounts":
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
		{"PROVIDER_ACCOUNTS", "provider_accounts"},
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
