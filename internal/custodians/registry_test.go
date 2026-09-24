package custodians

import (
	"sort"
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

// The registry must name exactly the instrument column's custodians (minus psp),
// and every kind must be drivable: a charge rail, a required credential, and a
// public value when a browser tokenizes against it — never one that is also a
// credential name (the public projection reads by name).
func TestRegistryIsClosedAndComplete(t *testing.T) {
	var want []string
	for _, c := range models.Custodians() {
		if c != models.CustodianPSP {
			want = append(want, c)
		}
	}
	sort.Strings(want)
	require.Equal(t, want, Kinds())

	for _, kind := range Kinds() {
		d, err := Require(" " + kind + " ")
		require.NoError(t, err)
		require.NotEmpty(t, d.ProxyRails, kind)
		secrets, required := map[string]bool{}, 0
		for _, s := range d.Secrets {
			secrets[s.Name] = true
			if s.Required {
				required++
			}
		}
		require.Positive(t, required, "%s: nothing authorizes detokenization", kind)
		public := 0
		for _, s := range d.Settings {
			if s.Public {
				public++
				require.NotEmpty(t, s.PublicField, "%s.%s", kind, s.Name)
				require.False(t, secrets[s.Name], "%s publishes credential name %s", kind, s.Name)
			}
		}
		if d.BrowserFlow != "" {
			require.Positive(t, public, "%s: browser flow with no public value", kind)
		}
	}
	_, err := Require("spreedly")
	require.ErrorContains(t, err, "unknown custodian kind")
}

func TestParseSettings(t *testing.T) {
	bt := models.CustodianBasisTheory
	got, err := ParseSettings(bt, map[string]any{" Public_API_Key ": " key_pub ", SettingNetworkTokens: true})
	require.NoError(t, err)
	require.Equal(t, Settings{PublicAPIKey: "key_pub", NetworkTokens: true}, got)

	// Paid add-ons are never armed by omission; the window has one default.
	got, err = ParseSettings(bt, map[string]any{SettingPublicAPIKey: "k"})
	require.NoError(t, err)
	require.False(t, got.NetworkTokens || got.AccountUpdater)
	require.Equal(t, DefaultAccountUpdaterLookaheadDays, got.LookaheadDays())

	// Manifest, env overlay and jsonb shapes of the same number resolve identically.
	for _, raw := range []any{21, int64(21), float64(21), " 21 "} {
		got, err := ParseSettings(bt, map[string]any{SettingPublicAPIKey: "k", SettingAccountUpdater: "true", SettingAccountUpdaterLookaheadDays: raw})
		require.NoError(t, err, "%T", raw)
		require.True(t, got.AccountUpdater)
		require.Equal(t, 21, got.LookaheadDays())
	}

	got, err = ParseSettings(models.CustodianHyperSwitch, map[string]any{SettingPublicAPIKey: "k", SettingProfileID: "prof"})
	require.NoError(t, err)
	require.Equal(t, "prof", got.ProfileID)

	for name, tc := range map[string]struct {
		kind     string
		settings map[string]any
	}{
		"unknown key":        {bt, map[string]any{SettingPublicAPIKey: "k", "invented_next_year": "x"}},
		"missing required":   {bt, map[string]any{SettingNetworkTokens: true}},
		"blank required":     {bt, map[string]any{SettingPublicAPIKey: "   "}},
		"wrong string type":  {bt, map[string]any{SettingPublicAPIKey: 7}},
		"wrong bool type":    {bt, map[string]any{SettingPublicAPIKey: "k", SettingNetworkTokens: 3}},
		"unparseable bool":   {bt, map[string]any{SettingPublicAPIKey: "k", SettingAccountUpdater: "yes please"}},
		"zero window":        {bt, map[string]any{SettingPublicAPIKey: "k", SettingAccountUpdaterLookaheadDays: 0}},
		"negative window":    {bt, map[string]any{SettingPublicAPIKey: "k", SettingAccountUpdaterLookaheadDays: -3}},
		"fractional window":  {bt, map[string]any{SettingPublicAPIKey: "k", SettingAccountUpdaterLookaheadDays: 1.5}},
		"prose window":       {bt, map[string]any{SettingPublicAPIKey: "k", SettingAccountUpdaterLookaheadDays: "a fortnight"}},
		"bool window":        {bt, map[string]any{SettingPublicAPIKey: "k", SettingAccountUpdaterLookaheadDays: true}},
		"hs without profile": {models.CustodianHyperSwitch, map[string]any{SettingPublicAPIKey: "k"}},
		"hs bt-only knob":    {models.CustodianHyperSwitch, map[string]any{SettingPublicAPIKey: "k", SettingProfileID: "p", SettingNetworkTokens: true}},
		"unknown kind":       {"spreedly", nil},
	} {
		_, err := ParseSettings(tc.kind, tc.settings)
		require.Error(t, err, name)
	}
}

func TestPublicSettingsProjection(t *testing.T) {
	d, _ := Get(models.CustodianBasisTheory)
	public, _, ok := d.PublicSettings(map[string]any{SettingPublicAPIKey: "key_pub", SettingNetworkTokens: true, SettingAccountUpdater: true})
	require.True(t, ok)
	require.Equal(t, map[string]string{"public_api_key": "key_pub"}, public, "non-public settings never leave the server")

	for _, settings := range []map[string]any{{SettingNetworkTokens: true}, {SettingPublicAPIKey: " "}, {SettingPublicAPIKey: 7}} {
		_, reason, ok := d.PublicSettings(settings)
		require.False(t, ok, "half-configured custodian must be withheld: %v", settings)
		require.NotEmpty(t, reason)
	}

	hs, _ := Get(models.CustodianHyperSwitch)
	public, _, ok = hs.PublicSettings(map[string]any{SettingPublicAPIKey: "pk", SettingProfileID: "profile-secretish"})
	require.True(t, ok)
	require.Equal(t, map[string]string{"public_api_key": "pk"}, public)
}

func TestSupportsRail(t *testing.T) {
	for _, kind := range Kinds() {
		d, _ := Get(kind)
		require.True(t, d.SupportsRail(" NMI "), kind)
		for _, rail := range []models.Rail{models.RailStripe, models.RailCCBill, models.RailSolana} {
			require.False(t, d.SupportsRail(rail), "%s must not claim a proxy charge path on %s", kind, rail)
		}
	}
}
