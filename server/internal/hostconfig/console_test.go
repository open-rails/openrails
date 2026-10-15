package hostconfig

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConsoleConfigurationUsesMerchantIssuers(t *testing.T) {
	bootEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "console.yaml"), "admin_console:\n  issuers:\n    example:\n      url: https://app.example/auth\n      client_id: billing-console\n      name: Example app\n")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "https://app.example/auth", cfg.AdminConsole.Issuers["example"].URL)
	require.Equal(t, "billing-console", cfg.AdminConsole.Issuers["example"].ClientID)
	require.False(t, cfg.LocalSignIn)
	require.False(t, cfg.AdminConsole.Disabled)
}

func TestConsoleCanBeDisabled(t *testing.T) {
	for _, source := range []string{"file", "environment"} {
		t.Run(source, func(t *testing.T) {
			bootEnv(t)
			var path string
			if source == "file" {
				path = writeFile(t, filepath.Join(t.TempDir(), "console.yaml"), "admin_console:\n  disabled: true\nlocal_sign_in: true\n")
			} else {
				require.Equal(t, "admin_console.disabled", envKeyToConfigKey("ADMIN_CONSOLE_DISABLED"))
				t.Setenv("ADMIN_CONSOLE_DISABLED", "true")
				t.Setenv("LOCAL_SIGN_IN", "true")
			}
			cfg, err := Load(path)
			require.NoError(t, err)
			require.NotNil(t, cfg.AdminConsole)
			require.True(t, cfg.AdminConsole.Disabled)
			require.True(t, cfg.LocalSignIn, "console availability does not select the sign-in policy")
		})
	}
}

func TestRetiredConsoleMountKeysAreRefused(t *testing.T) {
	for key, value := range map[string]string{"enabled": "false", "path": "/staff", "issuer": "{}"} {
		t.Run(key, func(t *testing.T) {
			bootEnv(t)
			path := writeFile(t, filepath.Join(t.TempDir(), "console.yaml"), "admin_console:\n  "+key+": "+value+"\n")
			_, err := Load(path)
			require.ErrorContains(t, err, "admin_console."+key)
			require.ErrorContains(t, err, "admin_console.issuers")
		})
	}
}
