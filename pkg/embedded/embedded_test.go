package embedded

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

// #745: embedded construction must declare Env and TestMode explicitly — no
// implicit dev-like defaulting the way standalone config.Load applies.

func TestApplyEmbeddedDefaultsRequiresExplicitEnv(t *testing.T) {
	cfg := &config.Config{TestMode: config.CredentialPostureLive}
	err := applyEmbeddedDefaults(cfg)
	require.Error(t, err)
	require.ErrorContains(t, err, "config.Env is required")

	cfg = &config.Config{Env: "   ", TestMode: config.CredentialPostureLive}
	require.ErrorContains(t, applyEmbeddedDefaults(cfg), "config.Env is required")
}

func TestApplyEmbeddedDefaultsRequiresExplicitTestMode(t *testing.T) {
	cfg := &config.Config{Env: "development"}
	err := applyEmbeddedDefaults(cfg)
	require.Error(t, err)
	require.ErrorContains(t, err, "config.TestMode is required")

	// Neither a dev-like Env nor a prod-like one may silently pick a posture.
	cfg = &config.Config{Env: "production"}
	require.ErrorContains(t, applyEmbeddedDefaults(cfg), "config.TestMode is required")
}

func TestApplyEmbeddedDefaultsAcceptsExplicitPosture(t *testing.T) {
	cfg := &config.Config{Env: "development", TestMode: config.CredentialPostureSandbox}
	require.NoError(t, applyEmbeddedDefaults(cfg))

	cfg = &config.Config{Env: "production", TestMode: config.CredentialPostureLive}
	require.NoError(t, applyEmbeddedDefaults(cfg))
}

// #742: a host that never set RateLimits/Captcha gets the same curated
// defaults config.Load applies — the embedded HTTP surface must not silently
// ship unthrottled.
func TestApplyEmbeddedDefaultsSeedsRateLimitsWhenNil(t *testing.T) {
	cfg := &config.Config{Env: "development", TestMode: config.CredentialPostureSandbox}
	require.NoError(t, applyEmbeddedDefaults(cfg))

	require.NotNil(t, cfg.RateLimits, "embedded construction must seed curated rate-limit defaults")
	require.Equal(t, config.GetDefaultBillingConfig().RateLimits, cfg.RateLimits)
	require.NotNil(t, cfg.Captcha, "embedded construction must seed the default captcha posture")
	require.Equal(t, config.CaptchaProviderTurnstile, cfg.Captcha.EffectiveProvider())
}

func TestApplyEmbeddedDefaultsLeavesHostRateLimitsAlone(t *testing.T) {
	custom := &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}}
	cfg := &config.Config{Env: "development", TestMode: config.CredentialPostureSandbox, RateLimits: custom}
	require.NoError(t, applyEmbeddedDefaults(cfg))
	require.Same(t, custom, cfg.RateLimits, "a host-supplied RateLimits must not be overwritten")
}

func TestApplyEmbeddedDefaultsExplicitDisableYieldsPassthrough(t *testing.T) {
	cfg := &config.Config{
		Env:                "development",
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
	_, err := New(Options{})
	require.ErrorContains(t, err, "config is required")
}

func TestNewRejectsUnsetPostureBeforeTouchingTheDatabase(t *testing.T) {
	cfg := &config.Config{} // no Env, no TestMode, no DB
	_, err := New(Options{Config: cfg, River: RiverManagedByOpenRails()})
	require.ErrorContains(t, err, "config.Env is required")

	cfg = &config.Config{Env: "development"}
	_, err = New(Options{Config: cfg, River: RiverManagedByOpenRails()})
	require.ErrorContains(t, err, "config.TestMode is required")
}

// #895: an undeclared River owner is refused BEFORE the posture checks and
// before any DB work — a host that forgot the fleet entirely must never get an
// engine back, because every read API would keep working while the money stops.
func TestNewRequiresRiverOwnership(t *testing.T) {
	cfg := &config.Config{Env: "development", TestMode: config.CredentialPostureSandbox}
	_, err := New(Options{Config: cfg})
	require.ErrorIs(t, err, ErrRiverRequired)

	// And the declaration is the ONLY thing that changes: with it, construction
	// proceeds far enough to reach the ordinary DB-dependent failure.
	_, err = New(Options{Config: cfg, River: RiverManagedByOpenRails()})
	require.NotErrorIs(t, err, ErrRiverRequired)
}
