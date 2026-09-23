package config

import (
	"testing"
	"time"

	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"
)

func TestNamingConfigUsesAuthKitDefaultsAndExplicitInputs(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name     string
		values   map[string]any
		enabled  bool
		interval time.Duration
		mode     authkit.FormerNameRetentionMode
		duration time.Duration
	}{
		{"defaults", nil, true, 72 * time.Hour, authkit.FormerNamesFinite, 90 * 24 * time.Hour},
		{"disabled zero forever", map[string]any{"auth.naming.enabled": false, "auth.naming.rename_interval": "0s", "auth.naming.former_names.mode": "forever"}, false, 0, authkit.FormerNamesForever, 0},
		{"finite duration only", map[string]any{"auth.naming.former_names.duration": "240h"}, true, 72 * time.Hour, authkit.FormerNamesFinite, 240 * time.Hour},
		{"immediate", map[string]any{"auth.naming.former_names.mode": "immediate"}, true, 72 * time.Hour, authkit.FormerNamesImmediate, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts []LoadOption
			for key, value := range tc.values {
				opts = append(opts, WithOverride(key, value))
			}
			cfg, err := Load("", opts...)
			require.NoError(t, err)
			policy, err := cfg.Auth.Naming.Normalize()
			require.NoError(t, err)
			require.Equal(t, tc.enabled, policy.Enabled)
			require.Equal(t, tc.interval, policy.RenameInterval)
			require.Equal(t, tc.mode, policy.FormerNameRetentionMode)
			require.Equal(t, tc.duration, policy.FormerNameRetention)
		})
	}
	_, err := Load("", WithOverride("auth.naming.rename_interval", "999999999999999999999h"))
	require.Error(t, err, "duration overflow must not turn into immediate rename")
}

func TestNamingEnvironmentKeepsExplicitFalseAndZero(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AUTH_NAMING_ENABLED", "false")
	t.Setenv("AUTH_NAMING_RENAME_INTERVAL", "0s")
	t.Setenv("AUTH_NAMING_FORMER_NAMES_MODE", "finite")
	t.Setenv("AUTH_NAMING_FORMER_NAMES_DURATION", "0s")
	cfg, err := Load("")
	require.NoError(t, err)
	require.NotNil(t, cfg.Auth.Naming.Enabled)
	require.False(t, *cfg.Auth.Naming.Enabled)
	require.NotNil(t, cfg.Auth.Naming.RenameInterval)
	require.Zero(t, *cfg.Auth.Naming.RenameInterval)
	policy, err := cfg.Auth.Naming.Normalize()
	require.NoError(t, err)
	require.Equal(t, authkit.FormerNamesImmediate, policy.FormerNameRetentionMode)
}
