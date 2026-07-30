package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
)

// Custody settings (or#879/or#880) — declared in a PSP's `settings` block:
//
//	merchants.<slug>.psps.<key>.<rail>.settings.custodian = basis_theory
//
// Custody is the axis orthogonal to the rail: the rail says WHO CHARGES the
// card, custody says WHO HOLDS it. Absent (or `psp`) means the PSP holds its
// own instruments — Stripe's pm_…, NMI's customer vault — which needs no
// configuration at all. A third-party custodian needs its own identity and
// credentials, and they hang off the PSP whose gateway the charges land on.
//
// The custodian's private API key is a PSP secret named `custodian_api_key`.
const (
	// PSPSettingCustodian names the custodian holding instruments charged
	// through this PSP. Absent = models.CustodianPSP.
	PSPSettingCustodian = "custodian"
	// PSPSettingCustodianAccountID is the custodian-native tenant identity
	// (Basis Theory: the tenant id). Operator-declared, no-whoami doctrine.
	PSPSettingCustodianAccountID = "custodian_account_id"
	// PSPSettingCustodianPublicAPIKey is the custodian's PUBLIC key served to
	// the checkout page (config, not a secret).
	PSPSettingCustodianPublicAPIKey = "custodian_public_api_key"
	// PSPSettingCustodianNetworkTokens arms network-token provisioning at
	// instrument creation (the $-add-on; never load-bearing — provisioning
	// failure warns and the instrument stays pan_proxy).
	PSPSettingCustodianNetworkTokens = "custodian_network_tokens"
)

// CustodianSecretAPIKey is the PSP secret slot holding the custodian's PRIVATE
// application key — the only custodial secret.
const CustodianSecretAPIKey = "custodian_api_key"

// retiredCustodySettingKeys fail LOUDLY rather than being ignored: they were
// keys of the `vaulted_card` fake rail, and silently accepting them would arm a
// PSP that charges through nothing.
var retiredCustodySettingKeys = map[string]string{
	"gateway_account": "removed (or#879): a vaulted_card account pointed at the NMI PSP it charged through. Declare custody ON that NMI PSP instead — settings.custodian=basis_theory — and drop this key",
	"nt_charges":      "removed (or#879): it was only ever a hard error (NMI documents no external DPAN+cryptogram acceptance). charge_via routing is decided per instrument, not per account",
	"api_key":         "not a setting: the custodian private application key is the PSP secret " + CustodianSecretAPIKey,
	"public_api_key":  "renamed to " + PSPSettingCustodianPublicAPIKey,
	"network_tokens":  "renamed to " + PSPSettingCustodianNetworkTokens,
}

var custodySettingKeys = map[string]bool{
	PSPSettingCustodian:              true,
	PSPSettingCustodianAccountID:     true,
	PSPSettingCustodianPublicAPIKey:  true,
	PSPSettingCustodianNetworkTokens: true,
}

// IsCustodySettingKey reports whether key belongs to the custody block — so a
// rail's own strict settings validator can leave it alone.
func IsCustodySettingKey(key string) bool {
	return custodySettingKeys[strings.ToLower(strings.TrimSpace(key))]
}

// CustodySettings is the typed view of a PSP's custody settings.
type CustodySettings struct {
	// Custodian is always stated: models.CustodianPSP when the PSP holds its
	// own instruments (the same no-absence rule as payment_methods.custodian).
	Custodian string
	// AccountID is the custodian-native tenant identity. "" for CustodianPSP.
	AccountID string
	// PublicAPIKey is checkout-page config (not a secret).
	PublicAPIKey string
	// NetworkTokens arms NT provisioning on instrument creation.
	NetworkTokens bool
}

// ThirdParty reports whether a custodian other than the PSP holds the card.
func (c CustodySettings) ThirdParty() bool {
	return c.Custodian != "" && c.Custodian != models.CustodianPSP
}

// ParseCustodySettings reads the custody block out of any PSP's settings map.
// Retired keys and unknown custodians error loudly (#651 — never silently pick
// a behavior); a settings map with no custody block yields CustodianPSP.
func ParseCustodySettings(settings map[string]any) (CustodySettings, error) {
	out := CustodySettings{Custodian: models.CustodianPSP}
	for key := range settings {
		norm := strings.ToLower(strings.TrimSpace(key))
		if why, retired := retiredCustodySettingKeys[norm]; retired {
			return CustodySettings{}, fmt.Errorf("psp settings: %q is %s", key, why)
		}
	}
	if raw, ok := settings[PSPSettingCustodian]; ok {
		v, err := custodySettingString(PSPSettingCustodian, raw)
		if err != nil {
			return CustodySettings{}, err
		}
		out.Custodian = strings.ToLower(strings.TrimSpace(v))
	}
	if !isDeclaredCustodian(out.Custodian) {
		return CustodySettings{}, fmt.Errorf("psp settings: unknown custodian %q (known: %s)", out.Custodian, strings.Join(models.Custodians(), ", "))
	}
	if raw, ok := settings[PSPSettingCustodianAccountID]; ok {
		v, err := custodySettingString(PSPSettingCustodianAccountID, raw)
		if err != nil {
			return CustodySettings{}, err
		}
		out.AccountID = strings.TrimSpace(v)
	}
	if raw, ok := settings[PSPSettingCustodianPublicAPIKey]; ok {
		v, err := custodySettingString(PSPSettingCustodianPublicAPIKey, raw)
		if err != nil {
			return CustodySettings{}, err
		}
		out.PublicAPIKey = strings.TrimSpace(v)
	}
	var err error
	if out.NetworkTokens, err = custodySettingBool(PSPSettingCustodianNetworkTokens, settings); err != nil {
		return CustodySettings{}, err
	}

	if !out.ThirdParty() {
		// A PSP that holds its own instruments must not carry custodian config:
		// half-declared custody is how an arrangement ends up armed by accident.
		for _, key := range []string{PSPSettingCustodianAccountID, PSPSettingCustodianPublicAPIKey, PSPSettingCustodianNetworkTokens} {
			if _, ok := settings[key]; ok {
				return CustodySettings{}, fmt.Errorf("psp settings: %s is only meaningful with a third-party %s (got %q)", key, PSPSettingCustodian, out.Custodian)
			}
		}
		return out, nil
	}
	if out.AccountID == "" {
		return CustodySettings{}, fmt.Errorf("psp settings: custodian %q requires %s (the custodian-native tenant id)", out.Custodian, PSPSettingCustodianAccountID)
	}
	return out, nil
}

func isDeclaredCustodian(v string) bool {
	for _, c := range models.Custodians() {
		if c == v {
			return true
		}
	}
	return false
}

// ValidateCustodySettings is the strict manifest-push variant: known keys must
// parse and any key that is neither custody nor rail-known fails the push.
func ValidateCustodySettings(settings map[string]any, railKnown func(string) bool) error {
	if _, err := ParseCustodySettings(settings); err != nil {
		return err
	}
	var unknown []string
	for key := range settings {
		norm := strings.ToLower(strings.TrimSpace(key))
		if custodySettingKeys[norm] {
			continue
		}
		if railKnown != nil && railKnown(norm) {
			continue
		}
		unknown = append(unknown, key)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	known := make([]string, 0, len(custodySettingKeys))
	for key := range custodySettingKeys {
		known = append(known, key)
	}
	sort.Strings(known)
	return fmt.Errorf("psp settings: unknown key(s) %s (custody keys: %s)", strings.Join(unknown, ", "), strings.Join(known, ", "))
}

func custodySettingBool(key string, settings map[string]any) (bool, error) {
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
			return false, fmt.Errorf("psp settings: %s must be a boolean (got %q)", key, v)
		}
		return b, nil
	default:
		return false, fmt.Errorf("psp settings: %s must be a boolean (got %T)", key, raw)
	}
}

func custodySettingString(key string, raw any) (string, error) {
	v, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("psp settings: %s must be a string (got %T)", key, raw)
	}
	return strings.TrimSpace(v), nil
}
