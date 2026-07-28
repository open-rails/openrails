package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// vaulted_card rail-account `settings` keys (#795). Declared under
// merchants.<slug>.accounts.<key>.vaulted_card.settings and stored on the
// psps row. account_id = the BT tenant id (operator-declared,
// no-whoami doctrine); the ONLY custodial secret is the private app key
// (secrets.api_key). The public app key is checkout-page config, not a secret.
const (
	// VaultedCardSettingGatewayAccount references the NMI rail account (by its
	// account_id, i.e. the NMI gateway id) that proxy charges land on —
	// destination credentials + DirectPost URL come from THAT account. Required.
	VaultedCardSettingGatewayAccount = "gateway_account"
	// VaultedCardSettingNetworkTokens arms NT provisioning on instrument
	// creation (the $-add-on; never load-bearing — provisioning failure warns
	// and the instrument stays pan_proxy).
	VaultedCardSettingNetworkTokens = "network_tokens"
	// VaultedCardSettingNTCharges arms charge_via=network_token routing. HARD
	// CONFIG ERROR on NMI gateway accounts: NMI documents no external
	// DPAN+cryptogram acceptance (#795 A9) — flipping this requires a gateway
	// that does.
	VaultedCardSettingNTCharges = "nt_charges"
	// VaultedCardSettingPublicAPIKey is the BT PUBLIC application key served
	// to the checkout page (config-not-secret).
	VaultedCardSettingPublicAPIKey = "public_api_key"
)

var vaultedCardKnownSettingKeys = map[string]bool{
	VaultedCardSettingGatewayAccount: true,
	VaultedCardSettingNetworkTokens:  true,
	VaultedCardSettingNTCharges:      true,
	VaultedCardSettingPublicAPIKey:   true,
}

// VaultedCardAccountSettings is the typed view of a vaulted_card rail-account
// settings map.
type VaultedCardAccountSettings struct {
	GatewayAccount string
	NetworkTokens  bool
	NTCharges      bool
	PublicAPIKey   string
}

// ParseVaultedCardAccountSettings parses the known keys; malformed values for
// known keys error loudly (#651 — never silently pick a behavior).
func ParseVaultedCardAccountSettings(settings map[string]any) (VaultedCardAccountSettings, error) {
	var out VaultedCardAccountSettings
	if raw, ok := settings[VaultedCardSettingGatewayAccount]; ok {
		v, err := settingString(VaultedCardSettingGatewayAccount, raw)
		if err != nil {
			return out, err
		}
		out.GatewayAccount = v
	}
	if raw, ok := settings[VaultedCardSettingPublicAPIKey]; ok {
		v, err := settingString(VaultedCardSettingPublicAPIKey, raw)
		if err != nil {
			return out, err
		}
		out.PublicAPIKey = v
	}
	var err error
	if out.NetworkTokens, err = settingBool(VaultedCardSettingNetworkTokens, settings); err != nil {
		return out, err
	}
	if out.NTCharges, err = settingBool(VaultedCardSettingNTCharges, settings); err != nil {
		return out, err
	}
	if strings.TrimSpace(out.GatewayAccount) == "" {
		return out, fmt.Errorf("vaulted_card settings: gateway_account is required (the NMI rail account_id proxy charges land on)")
	}
	// #795 A9: no documented external network-token acceptance on NMI. The
	// gateway reference IS an NMI account today, so nt_charges is a hard
	// config error until a gateway that accepts external NTs exists.
	if out.NTCharges {
		return out, fmt.Errorf("vaulted_card settings: nt_charges=true is not supported — gateway account %q is an NMI gateway and NMI documents no external network-token (DPAN+cryptogram) acceptance; charge_via stays pan_proxy", out.GatewayAccount)
	}
	return out, nil
}

func settingBool(key string, settings map[string]any) (bool, error) {
	raw, ok := settings[key]
	if !ok {
		return false, nil
	}
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return false, fmt.Errorf("vaulted_card settings: %s must be a boolean (got %q)", key, v)
		}
		return b, nil
	default:
		return false, fmt.Errorf("vaulted_card settings: %s must be a boolean (got %T)", key, raw)
	}
}

// ValidateVaultedCardAccountSettings is the strict manifest-push variant:
// known keys must parse and unknown keys fail the push loudly.
func ValidateVaultedCardAccountSettings(settings map[string]any) error {
	if _, err := ParseVaultedCardAccountSettings(settings); err != nil {
		return err
	}
	var unknown []string
	for key := range settings {
		if !vaultedCardKnownSettingKeys[strings.ToLower(strings.TrimSpace(key))] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		known := make([]string, 0, len(vaultedCardKnownSettingKeys))
		for key := range vaultedCardKnownSettingKeys {
			known = append(known, key)
		}
		sort.Strings(known)
		return fmt.Errorf("vaulted_card settings: unknown key(s) %s (known: %s)", strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	return nil
}
