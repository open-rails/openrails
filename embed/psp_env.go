package embed

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// PSPFromEnv declares one PSP from conventionally named variables, so enabling
// a provider in a host is configuration only. With prefix P = upper(key)+"_":
// P+"RAIL" (default: key), P+"ACCOUNT_ID", then P+upper(name) for each of the
// rail's credential slots and scalar settings. Required slots must be set;
// unset optional ones are omitted.
func PSPFromEnv(key string, lookup func(string) (string, bool)) (PSPConfig, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return nil, fmt.Errorf("PSP key is required")
	}
	prefix := strings.ToUpper(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, key)) + "_"
	get := func(name string) string {
		value, _ := lookup(prefix + name)
		return strings.TrimSpace(value)
	}
	rail := strings.ToLower(get("RAIL"))
	if rail == "" {
		rail = key
	}
	descriptor, ok := rails.Lookup(models.Rail(rail))
	if !ok || !descriptor.HasPSPs {
		return nil, fmt.Errorf("PSP %q: unknown rail %q (set %sRAIL)", key, rail, prefix)
	}
	if descriptor.Rail == models.RailSolana {
		return nil, fmt.Errorf("PSP %q: solana PSPs are declared programmatically", key)
	}
	account := ProviderRailAccountConfig{AccountID: get("ACCOUNT_ID")}
	if account.AccountID == "" {
		return nil, fmt.Errorf("PSP %q requires %sACCOUNT_ID", key, prefix)
	}
	for _, slot := range descriptor.CredentialKeys {
		name := strings.ToUpper(slot.Name)
		value := get(name)
		if value == "" {
			if slot.Required {
				return nil, fmt.Errorf("PSP %q requires %s%s", key, prefix, name)
			}
			continue
		}
		if account.Secrets == nil {
			account.Secrets = map[string]string{}
		}
		account.Secrets[slot.Name] = value
	}
	for _, setting := range descriptor.SettingKeys {
		if value := get(strings.ToUpper(setting)); value != "" {
			if account.Settings == nil {
				account.Settings = map[string]any{}
			}
			account.Settings[setting] = value
		}
	}
	return PSPConfig{string(descriptor.Rail): account}, nil
}
