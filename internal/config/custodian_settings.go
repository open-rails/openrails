package config

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/custodians"
)

// Custody declaration (or#880 phase 3).
//
// A custodian is declared ONCE, as an account the merchant holds with a
// third-party vault:
//
//	merchants.<slug>.custodians.<key>
//	    kind:           basis_theory
//	    account_id:     <custodian-native tenant id>
//	    public_api_key: …
//	    network_tokens: false
//	    secrets:        { api_key: … }
//
// and each PSP whose gateway those cards are charged through REFERENCES it:
//
//	merchants.<slug>.psps.<key>.custodian: <custodian key>

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
}

// ValidateCustodianEntry is THE custodian validator, shared by the manifest
// plane and the store plane. Everything it checks is registry data, so a second
// vendor is a Descriptor and not another branch here.
func ValidateCustodianEntry(e CustodianEntry) error {
	key := strings.TrimSpace(e.Key)
	if key == "" {
		return fmt.Errorf("custodian: key is required")
	}
	if strings.EqualFold(key, PlatformCustodianKey) {
		return fmt.Errorf("custodian %q: the key is reserved for the platform vault's identity", key)
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
