package merchants

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
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
		if !d.HasRailMerchantAccounts {
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
	config.VaultedCardSettingGatewayAccount,
	config.VaultedCardSettingNetworkTokens,
	config.VaultedCardSettingNTCharges,
}

// TestPublicPSPConfigForServesOnlyWhitelistedSettings poisons a PSP's settings
// blob with every credential-key name, every known private settings key, and a
// key nobody has invented yet — then asserts the projection carries exactly the
// whitelisted fields and none of the poison VALUES.
func TestPublicPSPConfigForServesOnlyWhitelistedSettings(t *testing.T) {
	const poison = "LEAKED-SENTINEL-VALUE"

	for _, d := range rails.All() {
		profile, armable := publicRailProfiles[string(d.Rail)]
		if !armable {
			continue
		}
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
		// The legitimately public ones get a recognizable value.
		want := map[string]string{}
		for _, s := range profile.Settings {
			settings[s.Setting] = "public-" + s.Setting
			want[s.Field] = "public-" + s.Setting
		}

		cfg, reason, ok := PublicPSPConfigFor(PSPScope{
			Rail:      string(d.Rail),
			Key:       "acct-key",
			AccountID: "operator-declared-account-id",
			Settings:  settings,
		})
		if !ok {
			t.Errorf("rail %q: fully-configured PSP was withheld (%s)", d.Rail, reason)
			continue
		}
		if len(cfg.Config) != len(want) {
			t.Errorf("rail %q: config has %d fields, whitelist declares %d: %v", d.Rail, len(cfg.Config), len(want), cfg.Config)
		}
		for field, value := range cfg.Config {
			if want[field] != value {
				t.Errorf("rail %q: config[%q] = %q, not a whitelisted public value", d.Rail, field, value)
			}
		}
		// Whole-document check: nothing anywhere in the projection may carry a
		// poisoned value, nor the operator-declared account id.
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("rail %q: marshal: %v", d.Rail, err)
		}
		if strings.Contains(string(encoded), poison) {
			t.Errorf("rail %q: projection leaked a poisoned value: %s", d.Rail, encoded)
		}
		if strings.Contains(string(encoded), "operator-declared-account-id") {
			t.Errorf("rail %q: projection leaked the account id: %s", d.Rail, encoded)
		}
	}
}

// TestPublicPSPConfigForWithholdsIncompletePSPs: no silent fabrication. A rail
// whose required public value is missing is omitted, never advertised with an
// empty or invented key.
func TestPublicPSPConfigForWithholdsIncompletePSPs(t *testing.T) {
	// NMI without a tokenization key cannot be driven from a browser.
	if _, reason, ok := PublicPSPConfigFor(PSPScope{Rail: string(models.RailNMI), Key: "mobius"}); ok {
		t.Error("NMI without tokenization_key must be withheld")
	} else if !strings.Contains(reason, "tokenization_key") {
		t.Errorf("reason = %q, want it to name tokenization_key", reason)
	}

	// With the key, the Collect.js URL falls back to the DECLARED rail constant.
	cfg, _, ok := PublicPSPConfigFor(PSPScope{
		Rail:     string(models.RailNMI),
		Key:      "mobius",
		Settings: map[string]any{"tokenization_key": "public-collect-key"},
	})
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
	if _, _, ok := PublicPSPConfigFor(PSPScope{Rail: "paypal"}); ok {
		t.Error("a rail with no public profile must be withheld")
	}

	// A keyless account reports the rail kind — the same selector checkout
	// accepts for a single-account rail. Nothing is invented.
	cfg, _, ok = PublicPSPConfigFor(PSPScope{Rail: string(models.RailStripe)})
	if !ok || cfg.Key != string(models.RailStripe) || cfg.Flow != FlowRedirect || len(cfg.Config) != 0 {
		t.Errorf("keyless stripe projection = %+v (ok=%v)", cfg, ok)
	}
}
