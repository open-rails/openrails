package merchants

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// TestPublicRailProfilesCoverEveryCatalogRail: a rail that participates in the
// PSP catalog can be armed, so it MUST have a browser profile or its armed
// accounts vanish silently from checkout discovery. rails.All() is the
// compile-time-complete registry, so adding a rail fails here until someone
// decides — deliberately — how a browser drives it.
func TestPublicRailProfilesCoverEveryCatalogRail(t *testing.T) {
	for _, d := range rails.All() {
		if !d.HasPSPs {
			// Not armable — must NOT have a profile either.
			if _, ok := publicRailProfiles[string(d.Rail)]; ok {
				t.Errorf("rail %q declares no PSP catalog participation but has a public profile", d.Rail)
			}
			continue
		}
		profile, ok := publicRailProfiles[string(d.Rail)]
		if !ok {
			t.Errorf("rail %q is armable but has no public checkout profile — its armed PSPs would never be advertised", d.Rail)
			continue
		}
		switch profile.Flow {
		case FlowTokenize, FlowRedirect, FlowWallet:
		default:
			t.Errorf("rail %q declares unknown browser flow %q", d.Rail, profile.Flow)
		}
	}
}

// TestPublicWhitelistNeverNamesACredentialKey is the structural half of the
// secret boundary: no rail may whitelist, as a PUBLIC setting, a name that any
// rail declares as a CREDENTIAL slot. Driven off rails.All() -> CredentialKeys,
// so a new secret added to the registry is checked here automatically.
func TestPublicWhitelistNeverNamesACredentialKey(t *testing.T) {
	credential := map[string]string{}
	for _, d := range rails.All() {
		for _, k := range d.CredentialKeys {
			credential[k.Name] = string(d.Rail)
		}
	}
	if len(credential) == 0 {
		t.Fatal("rails registry declared no credential keys — this test would prove nothing")
	}
	for rail, profile := range publicRailProfiles {
		for _, s := range profile.Settings {
			if owner, bad := credential[s.Setting]; bad {
				t.Errorf("rail %q whitelists %q as public, but %q declares it as a credential (secret) key",
					rail, s.Setting, owner)
			}
			if owner, bad := credential[s.Field]; bad {
				t.Errorf("rail %q publishes wire field %q, which %q declares as a credential (secret) key",
					rail, s.Field, owner)
			}
		}
	}
}

// privateSettingKeys are settings keys that live on the SAME psps.settings blob
// as the public ones and must never be served. rpc_api_key is the sharp one: a
// paid Helius key that #352 says must never reach a browser.
var privateSettingKeys = []string{
	config.SolanaSettingRPCProvider,
	config.SolanaSettingRPCAPIKey,
	config.SolanaSettingRecipientWallet,
}

// Custody settings are deliberately NOT in the list above: since or#880 they
// live on the CUSTODIAN's own row, not in the PSP settings blob. The custody
// case in the test below poisons the custodian's tenant id inside a VALID
// custodian declaration, which is the leak that actually needs proving.

// TestPublicPSPConfigForServesOnlyWhitelistedSettings poisons a PSP's settings
// blob with every credential-key name, every known private settings key, and a
// key nobody has invented yet — then asserts the projection carries exactly the
// whitelisted fields and none of the poison VALUES.
func TestPublicPSPConfigForServesOnlyWhitelistedSettings(t *testing.T) {
	const poison = "LEAKED-SENTINEL-VALUE"

	type projectionCase struct {
		name    string
		rail    models.Rail
		profile railPublicProfile
		// custodian is the valid custodian the PSP references, if any
		// (or#880): custody OVERRIDES the rail profile, so it gets its own case.
		custodian *CustodianScope
	}
	var cases []projectionCase
	for _, d := range rails.All() {
		if profile, armable := publicRailProfiles[string(d.Rail)]; armable {
			cases = append(cases, projectionCase{string(d.Rail), d.Rail, profile, nil})
		}
	}
	// A custodian-held NMI PSP: the browser tokenizes against the CUSTODIAN, so
	// the projection must serve the custodian's public key and nothing else.
	// The custodian's tenant id is poisoned to prove it never ships.
	btDescriptor, ok := custodians.Get(models.CustodianBasisTheory)
	if !ok {
		t.Fatal("basis_theory must be a declared custodian kind")
	}
	btProfile := railPublicProfile{Flow: btDescriptor.BrowserFlow}
	for _, slot := range btDescriptor.Settings {
		if slot.Public {
			btProfile.Settings = append(btProfile.Settings, publicSetting{
				Setting: slot.Name, Field: slot.PublicField, Required: slot.Required,
			})
		}
	}
	cases = append(cases, projectionCase{
		name:    "nmi+basis_theory",
		rail:    models.RailNMI,
		profile: btProfile,
		custodian: &CustodianScope{
			ID:          uuid.New(),
			Key:         "bt",
			Kind:        models.CustodianBasisTheory,
			Environment: "test",
			AccountID:   poison,
			Settings: map[string]any{
				custodians.SettingNetworkTokens: true,
			},
		},
	})

	for _, tc := range cases {
		d, profile := tc, tc.profile
		settings := map[string]any{}
		// Every credential slot ANY rail declares, as if an operator had
		// mistakenly stuffed a secret into the settings blob.
		for _, other := range rails.All() {
			for _, k := range other.CredentialKeys {
				settings[k.Name] = poison
			}
		}
		for _, k := range privateSettingKeys {
			settings[k] = poison
		}
		settings["a_key_invented_next_year"] = poison
		// The legitimately public ones get a recognizable value. A custodian's
		// public settings live on ITS row, so they are poisoned/served there.
		want := map[string]string{}
		for _, s := range profile.Settings {
			if tc.custodian != nil {
				tc.custodian.Settings[s.Setting] = "public-" + s.Setting
			} else {
				settings[s.Setting] = "public-" + s.Setting
			}
			want[s.Field] = "public-" + s.Setting
		}
		// A custodian displaces the rail's own tokenizer keys; declaring both
		// is refused, so drop them from the poison set in that case.
		var custodianID *uuid.UUID
		if tc.custodian != nil {
			delete(settings, "tokenization_key")
			delete(settings, "tokenization_url")
			custodianID = &tc.custodian.ID
		}

		cfg, reason, ok := PublicPSPConfigFor(PSPScope{
			Rail:        string(tc.rail),
			Key:         "acct-key",
			AccountID:   "operator-declared-account-id",
			Settings:    settings,
			CustodianID: custodianID,
		}, tc.custodian)
		if !ok {
			t.Errorf("%s: fully-configured PSP was withheld (%s)", d.name, reason)
			continue
		}
		if len(cfg.Config) != len(want) {
			t.Errorf("%s: config has %d fields, whitelist declares %d: %v", d.name, len(cfg.Config), len(want), cfg.Config)
		}
		for field, value := range cfg.Config {
			if want[field] != value {
				t.Errorf("%s: config[%q] = %q, not a whitelisted public value", d.name, field, value)
			}
		}
		// Whole-document check: nothing anywhere in the projection may carry a
		// poisoned value, nor the operator-declared account id.
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("%s: marshal: %v", d.name, err)
		}
		if strings.Contains(string(encoded), poison) {
			t.Errorf("%s: projection leaked a poisoned value: %s", d.name, encoded)
		}
		if strings.Contains(string(encoded), "operator-declared-account-id") {
			t.Errorf("%s: projection leaked the account id: %s", d.name, encoded)
		}
	}
}

// TestPublicPSPConfigForWithholdsIncompletePSPs: no silent fabrication. A rail
// whose required public value is missing is omitted, never advertised with an
// empty or invented key.
func TestPublicPSPConfigForWithholdsIncompletePSPs(t *testing.T) {
	// NMI without a tokenization key cannot be driven from a browser.
	if _, reason, ok := PublicPSPConfigFor(PSPScope{Rail: string(models.RailNMI), Key: "mobius"}, nil); ok {
		t.Error("NMI without tokenization_key must be withheld")
	} else if !strings.Contains(reason, "tokenization_key") {
		t.Errorf("reason = %q, want it to name tokenization_key", reason)
	}

	// With the key, the Collect.js URL falls back to the DECLARED rail constant.
	cfg, _, ok := PublicPSPConfigFor(PSPScope{
		Rail:     string(models.RailNMI),
		Key:      "mobius",
		Settings: map[string]any{"tokenization_key": "public-collect-key"},
	}, nil)
	if !ok {
		t.Fatal("NMI with a tokenization key must be advertised")
	}
	if cfg.Config["tokenization_url"] != DefaultNMICollectJSURL {
		t.Errorf("tokenization_url = %q, want the declared default", cfg.Config["tokenization_url"])
	}
	if cfg.Flow != FlowTokenize || cfg.Key != "mobius" || cfg.Rail != string(models.RailNMI) {
		t.Errorf("unexpected projection: %+v", cfg)
	}

	// A rail with no browser profile at all is withheld, not guessed at.
	if _, _, ok := PublicPSPConfigFor(PSPScope{Rail: "paypal"}, nil); ok {
		t.Error("a rail with no public profile must be withheld")
	}

	// A keyless account reports the rail kind — the same selector checkout
	// accepts for a single-account rail. Nothing is invented.
	cfg, _, ok = PublicPSPConfigFor(PSPScope{Rail: string(models.RailStripe)}, nil)
	if !ok || cfg.Key != string(models.RailStripe) || cfg.Flow != FlowRedirect || len(cfg.Config) != 0 {
		t.Errorf("keyless stripe projection = %+v (ok=%v)", cfg, ok)
	}
}
