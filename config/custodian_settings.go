package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/custodians"
)

// Custody declaration (or#880 phase 3).
//
// A custodian is declared ONCE, as an account the merchant holds with a
// third-party vault:
//
//	merchants.<slug>.custodians.<key>.<kind>
//	    account_id: <custodian-native tenant id>
//	    settings:   { public_api_key: …, network_tokens: false }
//	    secrets:    { api_key: … }
//
// and each PSP whose gateway those cards are charged through REFERENCES it:
//
//	merchants.<slug>.psps.<key>.<rail>.custodian: <custodian key>
//
// Phase 2 kept the whole arrangement inside each PSP's settings map. That was
// right about where custody hangs and wrong about whose credentials those are:
// copied per PSP, one custodian silently became two.

// retiredPSPCustodySettingKeys fail LOUDLY rather than being ignored. Every one
// of them named a credential that now belongs to the custodian entry, and a
// custody knob accepted-but-inert sits on a money path reading as "this works".
var retiredPSPCustodySettingKeys = map[string]string{
	"custodian":                "moved (or#880): custody is a REFERENCE now — declare merchants.<slug>.custodians.<key>.<kind> once and point this PSP at it with `custodian: <key>` on the PSP entry (a sibling of account_id), not a settings key",
	"custodian_account_id":     "moved (or#880): the custodian-native tenant id is the custodian entry's account_id",
	"custodian_public_api_key": "moved (or#880): declare it as the custodian entry's settings." + custodians.SettingPublicAPIKey,
	"custodian_network_tokens": "moved (or#880): declare it as the custodian entry's settings." + custodians.SettingNetworkTokens,
	"custodian_api_key":        "moved (or#880): the custodian's private application key is the custodian entry's `" + custodians.SecretAPIKey + "` secret, not a PSP setting",
	// or#879's own retirements, still loud: they were keys of the fake
	// `vaulted_card` rail, and accepting one would arm a PSP charging nothing.
	"gateway_account": "removed (or#879): a vaulted_card account pointed at the NMI PSP it charged through. Declare the custodian once and reference it from that NMI PSP instead",
	"nt_charges":      "removed (or#879): it was only ever a hard error (NMI documents no external DPAN+cryptogram acceptance). charge_via routing is decided per instrument, not per account",
}

// RejectRetiredCustodySettings refuses a PSP settings map that still carries an
// inline custody block. It runs on both ingestion planes — manifest push and
// stored-settings resolution — so neither can arm one.
func RejectRetiredCustodySettings(settings map[string]any) error {
	var retired []string
	for key := range settings {
		if _, ok := retiredPSPCustodySettingKeys[strings.ToLower(strings.TrimSpace(key))]; ok {
			retired = append(retired, key)
		}
	}
	if len(retired) == 0 {
		return nil
	}
	sort.Strings(retired)
	first := strings.ToLower(strings.TrimSpace(retired[0]))
	return fmt.Errorf("psp settings: %q is %s", retired[0], retiredPSPCustodySettingKeys[first])
}

// CustodianEntry is ONE declared custodian, in the shape both ingestion planes
// converge on: the manifest block and a mode-2 API write validate through
// exactly this struct, so a value one plane accepts is never one the other
// silently drops.
type CustodianEntry struct {
	// Key is the merchant's name for this custodian (custodians.key) — the
	// value a PSP's `custodian:` field references.
	Key string
	// Kind is the vendor (custodians registry): basis_theory today.
	Kind string
	// AccountID is the custodian-native tenant identity. Operator-declared —
	// there is no runtime whoami (#592).
	AccountID string
	// Settings are the declared non-secret knobs, validated against Kind.
	Settings map[string]any
	// Archived drains the custodian: instruments it holds stay chargeable, no
	// new arrangement may reference it.
	Archived bool
	// SecretKeys are the secret slots the declaration supplies values for.
	// Validation only — the values themselves never enter this struct.
	SecretKeys []string
	// CredentialVersions carries or#812 rotation watermarks for the slots this
	// write rotated. Empty on the manifest plane, which seeds rather than
	// rotates — an empty map never clears a floor another writer recorded.
	CredentialVersions map[string]int
}

// ValidateCustodianEntry is THE custodian validator, shared by the manifest
// plane and the store plane. Everything it checks is registry data, so a second
// vendor is a Descriptor and not another branch here.
func ValidateCustodianEntry(e CustodianEntry) error {
	key := strings.TrimSpace(e.Key)
	if key == "" {
		return fmt.Errorf("custodian: key is required")
	}
	d, err := custodians.Require(e.Kind)
	if err != nil {
		return fmt.Errorf("custodian %q: %w", key, err)
	}
	if strings.TrimSpace(e.AccountID) == "" {
		return fmt.Errorf("custodian %q (%s): account_id is required (the custodian-native tenant id)", key, d.Kind)
	}
	if _, err := custodians.ParseSettings(d.Kind, e.Settings); err != nil {
		return fmt.Errorf("custodian %q: %w", key, err)
	}
	declared := map[string]bool{}
	for _, name := range e.SecretKeys {
		name = strings.ToLower(strings.TrimSpace(name))
		if _, ok := d.Secret(name); !ok {
			return fmt.Errorf("custodian %q (%s): unknown secret %q (known: %s)", key, d.Kind, name, strings.Join(d.SecretNames(), ", "))
		}
		declared[name] = true
	}
	if e.SecretKeys == nil {
		// A caller that does not carry the secret set (store plane, where
		// secrets live in the secret store and are checked at arm time) skips
		// the requiredness check rather than failing on an unknown.
		return nil
	}
	for _, slot := range d.Secrets {
		if slot.Required && !declared[slot.Name] {
			return fmt.Errorf("custodian %q (%s): secret %s is required (its private application key)", key, d.Kind, slot.Name)
		}
	}
	return nil
}
