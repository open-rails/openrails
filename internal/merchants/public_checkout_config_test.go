package merchants

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/stretchr/testify/require"
)

// Every armable rail needs a browser profile (or its armed PSPs vanish from
// discovery), and no profile may publish a name any rail declares a secret.
func TestPublicRailProfilesAreCompleteAndSecretFree(t *testing.T) {
	credential := map[string]bool{}
	for _, d := range rails.All() {
		for _, k := range d.CredentialKeys {
			credential[k.Name] = true
		}
	}
	require.NotEmpty(t, credential)
	for _, d := range rails.All() {
		profile, ok := publicRailProfiles[string(d.Rail)]
		require.Equal(t, d.HasPSPs, ok, "rail %s: profile iff armable", d.Rail)
		if !ok {
			continue
		}
		require.Contains(t, []string{FlowTokenize, FlowRedirect, FlowWallet}, profile.Flow, d.Rail)
		for _, s := range profile.Settings {
			require.False(t, credential[s.Setting], "%s publishes credential setting %s", d.Rail, s.Setting)
			require.False(t, credential[s.Field], "%s publishes credential field %s", d.Rail, s.Field)
		}
	}
}

// The projection is a whitelist: poison the settings blob with every secret
// name, every private setting and an unknown key, and only whitelisted values
// (never the poison or the account id) may be served.
func TestPublicPSPConfigServesOnlyWhitelistedSettings(t *testing.T) {
	const poison = "LEAKED-SENTINEL-VALUE"
	poisoned := func() map[string]any {
		s := map[string]any{"a_key_invented_next_year": poison, config.SolanaSettingRPCProvider: poison, config.SolanaSettingRPCAPIKey: poison, config.SolanaSettingRecipientWallet: poison}
		for _, d := range rails.All() {
			for _, k := range d.CredentialKeys {
				s[k.Name] = poison
			}
		}
		return s
	}
	type projection struct {
		rail      string
		settings  []publicSetting
		custodian *CustodianScope
	}
	var cases []projection
	for rail, profile := range publicRailProfiles {
		cases = append(cases, projection{rail: rail, settings: profile.Settings})
	}
	bt, ok := custodians.Get(models.CustodianBasisTheory)
	require.True(t, ok)
	var btPublic []publicSetting
	for _, slot := range bt.Settings {
		if slot.Public {
			btPublic = append(btPublic, publicSetting{Setting: slot.Name, Field: slot.PublicField})
		}
	}
	cases = append(cases, projection{rail: string(models.RailNMI), settings: btPublic, custodian: &CustodianScope{
		ID: uuid.New(), Key: "bt", Kind: models.CustodianBasisTheory, Environment: "test", AccountID: poison,
		Settings: map[string]any{custodians.SettingNetworkTokens: true},
	}})

	for _, tc := range cases {
		settings, want := poisoned(), map[string]string{}
		for _, s := range tc.settings {
			value := "public-" + s.Setting
			if s.Allowed != nil {
				value = "https://secure.nmi.com/token/Collect.js"
			}
			if tc.custodian != nil {
				tc.custodian.Settings[s.Setting] = value
			} else {
				settings[s.Setting] = value
			}
			want[s.Field] = value
		}
		scope := PSPScope{Rail: tc.rail, Key: "acct-key", AccountID: "operator-declared-account-id", Settings: settings}
		if tc.custodian != nil {
			scope.CustodianID = &tc.custodian.ID
		}
		cfg, reason, ok := PublicPSPConfigFor(scope, tc.custodian)
		require.True(t, ok, "%s: %s", tc.rail, reason)
		if len(want) == 0 {
			want = nil
		}
		require.Equal(t, want, cfg.Config, tc.rail)
		encoded, err := json.Marshal(cfg)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), poison, tc.rail)
		require.NotContains(t, string(encoded), "operator-declared-account-id", tc.rail)
	}
}

// Never fabricate: an incomplete or unresolvable PSP is withheld with a reason.
func TestPublicPSPConfigWithholdsUndrivablePSPs(t *testing.T) {
	bt := &CustodianScope{ID: uuid.New(), Key: "bt", Kind: models.CustodianBasisTheory, Settings: map[string]any{custodians.SettingPublicAPIKey: "pub"}}
	archived := *bt
	archived.Archived = true
	for name, tc := range map[string]struct {
		scope     PSPScope
		custodian *CustodianScope
		reason    string
	}{
		"nmi without tokenization key": {PSPScope{Rail: "nmi", Key: "mobius"}, nil, "tokenization_key"},
		"unknown rail":                 {PSPScope{Rail: "unknown"}, nil, "profile"},
		"retired inline custody":       {PSPScope{Rail: "nmi", Settings: map[string]any{"tokenization_key": "tk", "custodian": "bt"}}, nil, "custodian"},
		"unresolved custodian":         {PSPScope{Rail: "nmi", CustodianID: &bt.ID}, nil, "custodian"},
		"archived custodian":           {PSPScope{Rail: "nmi", CustodianID: &bt.ID}, &archived, "archived"},
		"custodian missing public key": {PSPScope{Rail: "nmi", CustodianID: &bt.ID}, &CustodianScope{Key: "bt", Kind: models.CustodianBasisTheory}, ""},
	} {
		_, reason, ok := PublicPSPConfigFor(tc.scope, tc.custodian)
		require.False(t, ok, name)
		require.Contains(t, reason, tc.reason, name)
	}

	for name, tc := range map[string]struct {
		scope     PSPScope
		custodian *CustodianScope
		want      PublicPSPConfig
	}{
		// Script URLs are never merchant-arbitrary: off-allowlist falls back to the declared default.
		"nmi": {PSPScope{Rail: " NMI ", Key: "Mobius", Settings: map[string]any{"tokenization_key": "tk", "tokenization_url": "https://evil.example/c.js"}}, nil,
			PublicPSPConfig{Key: "mobius", Rail: "nmi", Flow: FlowTokenize, Config: map[string]string{"tokenization_key": "tk", "tokenization_url": DefaultNMICollectJSURL}}},
		"keyless stripe redirects": {PSPScope{Rail: "stripe"}, nil, PublicPSPConfig{Key: "stripe", Rail: "stripe", Flow: FlowRedirect}},
		"stripe publishable key enables elements": {PSPScope{Rail: "stripe", Settings: map[string]any{"publishable_key": "pk_test_abc", "secret_key": "sk_test_x"}}, nil,
			PublicPSPConfig{Key: "stripe", Rail: "stripe", Flow: FlowElements, Config: map[string]string{"publishable_key": "pk_test_abc"}}},
		"custodian overrides rail tokenizer": {PSPScope{Rail: "nmi", CustodianID: &bt.ID, Settings: map[string]any{"tokenization_key": "rail-tk"}}, bt,
			PublicPSPConfig{Key: "nmi", Rail: "nmi", Flow: FlowTokenize, Custodian: models.CustodianBasisTheory, Config: map[string]string{"public_api_key": "pub"}}},
	} {
		got, reason, ok := PublicPSPConfigFor(tc.scope, tc.custodian)
		require.True(t, ok, "%s: %s", name, reason)
		require.Equal(t, tc.want.Key, got.Key, name)
		require.Equal(t, tc.want.Rail, got.Rail, name)
		require.Equal(t, tc.want.Flow, got.Flow, name)
		require.Equal(t, tc.want.Config, got.Config, name)
		if tc.want.Custodian != "" {
			require.Equal(t, tc.want.Custodian, got.Custodian, name)
		} else {
			require.Equal(t, models.CustodianPSP, got.Custodian, name)
		}
	}
}
