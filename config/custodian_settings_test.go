package config

import (
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
)

// or#880 phase 3: the phase-2 inline shape is RETIRED, and every retired key
// fails with a message that says where the value moved. Silently ignoring one
// would leave a custody knob stored inert on a money path — which reads as
// "this works" to the next operator.
func TestRejectRetiredCustodySettings(t *testing.T) {
	for _, key := range []string{
		"custodian",
		"custodian_account_id",
		"custodian_public_api_key",
		"custodian_network_tokens",
		"custodian_api_key",
		"gateway_account",
		"nt_charges",
	} {
		err := RejectRetiredCustodySettings(map[string]any{key: "whatever"})
		if err == nil {
			t.Errorf("%s: expected a loud rejection, got none", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: error does not name the key: %v", key, err)
		}
	}

	// Case and whitespace are not an escape hatch.
	if err := RejectRetiredCustodySettings(map[string]any{"  Custodian_Account_ID ": "x"}); err == nil {
		t.Error("a retired key must be refused regardless of case or padding")
	}

	// A rail's own settings are untouched — custody is not the only thing in
	// this map, and rejecting a live PSP's tokenizer key would be a worse bug.
	if err := RejectRetiredCustodySettings(map[string]any{
		"tokenization_key": "k", "tokenization_url": "u", "rpc_provider": "helius",
	}); err != nil {
		t.Errorf("rail settings must pass untouched: %v", err)
	}
	if err := RejectRetiredCustodySettings(nil); err != nil {
		t.Errorf("an empty settings map must pass: %v", err)
	}
}

func TestValidateCustodianEntry(t *testing.T) {
	valid := CustodianEntry{
		Key:       "bt",
		Kind:      models.CustodianBasisTheory,
		AccountID: "tnt_test",
		Settings: map[string]any{
			custodians.SettingPublicAPIKey:  "key_pub",
			custodians.SettingNetworkTokens: false,
		},
		SecretKeys: []string{custodians.SecretAPIKey},
	}
	if err := ValidateCustodianEntry(valid); err != nil {
		t.Fatalf("valid entry: %v", err)
	}

	mutate := func(f func(*CustodianEntry)) CustodianEntry {
		e := valid
		e.Settings = map[string]any{}
		for k, v := range valid.Settings {
			e.Settings[k] = v
		}
		e.SecretKeys = append([]string(nil), valid.SecretKeys...)
		f(&e)
		return e
	}

	for name, entry := range map[string]CustodianEntry{
		"no key":           mutate(func(e *CustodianEntry) { e.Key = " " }),
		"unknown kind":     mutate(func(e *CustodianEntry) { e.Kind = "hyperswitch" }),
		"no account id":    mutate(func(e *CustodianEntry) { e.AccountID = "" }),
		"no public key":    mutate(func(e *CustodianEntry) { delete(e.Settings, custodians.SettingPublicAPIKey) }),
		"unknown setting":  mutate(func(e *CustodianEntry) { e.Settings["invented"] = 1 }),
		"unknown secret":   mutate(func(e *CustodianEntry) { e.SecretKeys = []string{"security_key"} }),
		"missing api key":  mutate(func(e *CustodianEntry) { e.SecretKeys = []string{} }),
		"wrong value type": mutate(func(e *CustodianEntry) { e.Settings[custodians.SettingNetworkTokens] = "yes please" }),
	} {
		if err := ValidateCustodianEntry(entry); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}

	// A caller that does not carry the secret set (the store plane, where
	// secrets are checked at arm time) skips requiredness rather than failing.
	storePlane := valid
	storePlane.SecretKeys = nil
	if err := ValidateCustodianEntry(storePlane); err != nil {
		t.Errorf("store-plane validation must not require the secret set: %v", err)
	}
}
