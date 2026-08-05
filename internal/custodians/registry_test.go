package custodians

import (
	"sort"
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
)

// The registry and the instrument column must name the SAME closed set. A kind
// declared in one and not the other is a custodian that can be configured but
// never recorded, or recorded but never armed.
func TestRegistryMatchesInstrumentCustodianValues(t *testing.T) {
	want := []string{}
	for _, c := range models.Custodians() {
		if c != models.CustodianPSP {
			want = append(want, c)
		}
	}
	sort.Strings(want)
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("registry kinds %v != instrument custodians %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("registry kinds %v != instrument custodians %v", got, want)
		}
	}
}

// Every declared kind must be drivable end to end: a rail that can charge its
// instruments, a required credential, and a browser flow if a page has to
// tokenize against it. A descriptor missing any of these is a custodian that
// looks configured and cannot take a payment.
func TestEveryDescriptorIsComplete(t *testing.T) {
	for _, kind := range Kinds() {
		d, ok := Get(kind)
		if !ok {
			t.Fatalf("Kinds() returned %q, which Get does not know", kind)
		}
		if len(d.ProxyRails) == 0 {
			t.Errorf("custodian %q names no rail that can charge its instruments", kind)
		}
		required := 0
		for _, s := range d.Secrets {
			if s.Required {
				required++
			}
		}
		if required == 0 {
			t.Errorf("custodian %q declares no required credential — nothing would authorize detokenization", kind)
		}
		if d.BrowserFlow == "" {
			continue
		}
		public := 0
		for _, s := range d.Settings {
			if !s.Public {
				continue
			}
			public++
			if s.PublicField == "" {
				t.Errorf("custodian %q setting %q is public with no wire field name", kind, s.Name)
			}
		}
		if public == 0 {
			t.Errorf("custodian %q declares a browser flow but no public value the page could use", kind)
		}
	}
}

// A public setting must never share a name with a credential slot: the public
// projection reads by name, so a collision would serve a secret.
func TestPublicSettingsNeverNameACredential(t *testing.T) {
	for _, kind := range Kinds() {
		d, _ := Get(kind)
		secrets := map[string]bool{}
		for _, s := range d.Secrets {
			secrets[s.Name] = true
		}
		for _, s := range d.Settings {
			if s.Public && secrets[s.Name] {
				t.Errorf("custodian %q publishes %q, which it also declares as a credential", kind, s.Name)
			}
		}
	}
}

func TestParseSettings(t *testing.T) {
	bt := models.CustodianBasisTheory

	got, err := ParseSettings(bt, map[string]any{
		SettingPublicAPIKey:  "key_pub",
		SettingNetworkTokens: true,
	})
	if err != nil {
		t.Fatalf("valid settings: %v", err)
	}
	if got.PublicAPIKey != "key_pub" || !got.NetworkTokens {
		t.Errorf("parsed = %+v", got)
	}

	// network_tokens is optional and defaults to off — a paid add-on is never
	// armed by omission.
	got, err = ParseSettings(bt, map[string]any{SettingPublicAPIKey: "key_pub"})
	if err != nil || got.NetworkTokens {
		t.Errorf("omitted network_tokens = %+v (err %v), want off", got, err)
	}

	for name, settings := range map[string]map[string]any{
		"unknown key":       {SettingPublicAPIKey: "k", "invented_next_year": "x"},
		"missing required":  {SettingNetworkTokens: true},
		"wrong bool type":   {SettingPublicAPIKey: "k", SettingNetworkTokens: 3},
		"wrong string type": {SettingPublicAPIKey: 7},
		"empty required":    {SettingPublicAPIKey: "   "},
	} {
		if _, err := ParseSettings(bt, settings); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}

	if _, err := ParseSettings("hyperswitch", nil); err == nil {
		t.Error("an unknown kind must fail loudly, not resolve to nothing")
	}
}

// or#795: the batch account updater is a CONTRACTED add-on with one window
// knob. Arming is never implied, the window has one default, and a
// misdeclared window fails loudly rather than silently becoming the default.
func TestParseAccountUpdaterSettings(t *testing.T) {
	bt := models.CustodianBasisTheory

	got, err := ParseSettings(bt, map[string]any{SettingPublicAPIKey: "key_pub"})
	if err != nil {
		t.Fatalf("valid settings: %v", err)
	}
	if got.AccountUpdater {
		t.Error("a paid add-on must never be armed by omission")
	}
	if got.LookaheadDays() != DefaultAccountUpdaterLookaheadDays {
		t.Errorf("lookahead default = %d, want %d", got.LookaheadDays(), DefaultAccountUpdaterLookaheadDays)
	}

	// Every shape a manifest / env overlay / jsonb blob produces for the same
	// declared number resolves identically.
	for name, raw := range map[string]any{"int": 21, "float": float64(21), "string": "21"} {
		got, err := ParseSettings(bt, map[string]any{
			SettingPublicAPIKey:                "key_pub",
			SettingAccountUpdater:              "true",
			SettingAccountUpdaterLookaheadDays: raw,
		})
		if err != nil {
			t.Fatalf("%s lookahead: %v", name, err)
		}
		if !got.AccountUpdater || got.LookaheadDays() != 21 {
			t.Errorf("%s lookahead parsed = %+v", name, got)
		}
	}

	for name, settings := range map[string]map[string]any{
		"zero window":     {SettingPublicAPIKey: "k", SettingAccountUpdaterLookaheadDays: 0},
		"negative window": {SettingPublicAPIKey: "k", SettingAccountUpdaterLookaheadDays: -3},
		"fractional":      {SettingPublicAPIKey: "k", SettingAccountUpdaterLookaheadDays: 1.5},
		"not a number":    {SettingPublicAPIKey: "k", SettingAccountUpdaterLookaheadDays: "a fortnight"},
		"wrong type":      {SettingPublicAPIKey: "k", SettingAccountUpdaterLookaheadDays: true},
	} {
		if _, err := ParseSettings(bt, settings); err == nil {
			t.Errorf("%s: expected a loud refusal, got none", name)
		}
	}
}

func TestPublicSettingsProjection(t *testing.T) {
	d, _ := Get(models.CustodianBasisTheory)
	public, _, ok := d.PublicSettings(map[string]any{
		SettingPublicAPIKey:  "key_pub",
		SettingNetworkTokens: true,
	})
	if !ok {
		t.Fatal("a complete custodian must project")
	}
	if public["public_api_key"] != "key_pub" {
		t.Errorf("public = %v", public)
	}
	if len(public) != 1 {
		t.Errorf("public carried a non-public setting: %v", public)
	}
	if _, _, ok := d.PublicSettings(map[string]any{SettingNetworkTokens: true}); ok {
		t.Error("a custodian with no public key must be withheld, not advertised half-configured")
	}
}

func TestSupportsRail(t *testing.T) {
	d, _ := Get(models.CustodianBasisTheory)
	if !d.SupportsRail(models.RailNMI) {
		t.Error("basis_theory must be chargeable through nmi")
	}
	for _, rail := range []models.Rail{models.RailStripe, models.RailCCBill, models.RailSolana} {
		if d.SupportsRail(rail) {
			t.Errorf("basis_theory must not claim a proxy charge path on %s", rail)
		}
	}
}
