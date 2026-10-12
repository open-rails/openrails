package hostconfig

import (
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
)

func TestConsoleConfigurationUsesMerchantIssuers(t *testing.T) {
	bootEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "console.yaml"), "admin_console:\n  issuers:\n    example:\n      url: https://app.example/auth\n      client_id: billing-console\n      name: Example app\n")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "https://app.example/auth", cfg.AdminConsole.Issuers["example"].URL)
	require.Equal(t, "billing-console", cfg.AdminConsole.Issuers["example"].ClientID)
	require.False(t, cfg.LocalSignIn)
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
