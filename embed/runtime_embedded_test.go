package embed

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

// Provider posture is explicit; secure runtime defaults need no environment label.
func TestApplyEmbeddedDefaultsWithoutEnvironmentLabel(t *testing.T) {
	for _, posture := range []config.CredentialPosture{config.CredentialPostureLive, config.CredentialPostureSandbox} {
		cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: posture}
		require.NoError(t, applyEmbeddedDefaults(cfg))
		require.False(t, cfg.RateLimitsDisabled)
		require.NotNil(t, cfg.RateLimits)
	}
}

func TestApplyEmbeddedDefaultsRequiresExplicitTestMode(t *testing.T) {
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}
	err := applyEmbeddedDefaults(cfg)
	require.Error(t, err)
	require.ErrorContains(t, err, "config.TestMode is required")

	// A second zero-value configuration must also refuse to guess.
	cfg = &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}
	require.ErrorContains(t, applyEmbeddedDefaults(cfg), "config.TestMode is required")
}

func TestApplyEmbeddedDefaultsAcceptsExplicitPosture(t *testing.T) {
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: config.CredentialPostureSandbox}
	require.NoError(t, applyEmbeddedDefaults(cfg))

	cfg = &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: config.CredentialPostureLive}
	require.NoError(t, applyEmbeddedDefaults(cfg))
}

// #742: a host that never set RateLimits/Captcha gets the same curated
// defaults config.Load applies — the embedded HTTP surface must not silently
// ship unthrottled.
func TestApplyEmbeddedDefaultsSeedsRateLimitsWhenNil(t *testing.T) {
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: config.CredentialPostureSandbox}
	require.NoError(t, applyEmbeddedDefaults(cfg))

	require.NotNil(t, cfg.RateLimits, "embedded construction must seed curated rate-limit defaults")
	require.Equal(t, config.GetDefaultBillingConfig().RateLimits, cfg.RateLimits)
	require.NotNil(t, cfg.Captcha, "embedded construction must seed the default captcha posture")
	require.Equal(t, config.CaptchaProviderTurnstile, cfg.Captcha.EffectiveProvider())
}

func TestApplyEmbeddedDefaultsLeavesHostRateLimitsAlone(t *testing.T) {
	custom := &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}}
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: config.CredentialPostureSandbox, RateLimits: custom}
	require.NoError(t, applyEmbeddedDefaults(cfg))
	require.Same(t, custom, cfg.RateLimits, "a host-supplied RateLimits must not be overwritten")
}

func TestApplyEmbeddedDefaultsExplicitDisableYieldsPassthrough(t *testing.T) {
	cfg := &config.Config{
		ProviderWriteMode:  config.ProviderWriteModeReadOnly,
		TestMode:           config.CredentialPostureSandbox,
		RateLimitsDisabled: true,
	}
	require.NoError(t, applyEmbeddedDefaults(cfg))
	require.Nil(t, cfg.RateLimits, "an explicit opt-out must leave RateLimitHTTP as a passthrough")
	require.Nil(t, cfg.Captcha)
}

// New() surfaces the same errors — asserted without a real DB, since the
// posture/defaults checks run before anything DB-dependent.
func TestNewRejectsMissingConfig(t *testing.T) {
	_, err := New(context.Background(), Options{})
	require.ErrorContains(t, err, "config is required")
}

func TestNewRejectsUnsetPostureBeforeTouchingTheDatabase(t *testing.T) {
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly} // no TestMode or database
	_, err := New(context.Background(), Options{Config: cfg, River: RiverManagedByOpenRails()})
	require.ErrorContains(t, err, "config.TestMode is required")

	cfg = &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}
	_, err = New(context.Background(), Options{Config: cfg, River: RiverManagedByOpenRails()})
	require.ErrorContains(t, err, "config.TestMode is required")
}

// Omitting River selects a managed fleet and reaches normal posture validation.
func TestNewDefaultsRiverOwnership(t *testing.T) {
	_, err := New(context.Background(), Options{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}})
	require.ErrorContains(t, err, "config.TestMode is required")
	_, err = New(context.Background(), Options{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}, River: RiverFromHost()})
	require.ErrorContains(t, err, "config.TestMode is required")
}

func TestHostRiverRejectsAutomaticStartupBeforeBinding(t *testing.T) {
	_, err := New(context.Background(), Options{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}, River: RiverFromHost(), RunWorkers: true})
	require.ErrorContains(t, err, "managed-only")
}
