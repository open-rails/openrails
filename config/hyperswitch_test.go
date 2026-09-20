package config

import (
	"testing"

	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestHyperSwitchEndpointsBelongToHostAndRequireSecureTransport(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		posture   CredentialPosture
		url       string
		ok        bool
	}{
		{"https", "dev", CredentialPostureLive, "https://vault.example.test", true},
		{"loopback fixture", "dev", CredentialPostureSandbox, "http://127.0.0.1:9099", true},
		{"loopback live", "dev", CredentialPostureLive, "http://127.0.0.1:9099", false},
		{"private target", "dev", CredentialPostureSandbox, "http://10.0.0.1", false},
		{"hostname http", "dev", CredentialPostureSandbox, "http://localhost", false},
		{"userinfo", "dev", CredentialPostureSandbox, "https://user:secret@vault.example.test", false},
		{"query", "dev", CredentialPostureSandbox, "https://vault.example.test?token=secret", false},
		{"fragment", "dev", CredentialPostureSandbox, "https://vault.example.test#secret", false},
		{"missing host", "dev", CredentialPostureSandbox, "https:///vault", false},
		{"missing", "dev", CredentialPostureSandbox, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := providerSandboxConfig(tc.posture, ProviderSandboxConfig{})
			cfg.Env = tc.env
			cfg.HyperSwitch = &HyperSwitchConfig{APIBaseURL: tc.url, SDKURL: tc.url}
			err := Validate(cfg)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	settings := map[string]any{custodians.SettingPublicAPIKey: "pk_fixture", custodians.SettingProfileID: "profile_fixture"}
	_, err := custodians.ParseSettings(models.CustodianHyperSwitch, settings)
	require.NoError(t, err)
	for _, key := range []string{"api_base_url", "sdk_url", "gateway_url", "network_tokens", "account_updater"} {
		settings[key] = "https://tenant-chosen.example.test"
		_, err = custodians.ParseSettings(models.CustodianHyperSwitch, settings)
		require.Error(t, err)
		delete(settings, key)
	}
}
