package middleware

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/config"
)

// FC-13 / or#865: evaluateCaptchaVerify dereferences deps.Verifier without a
// nil check because enforcement implies captcha is enabled and an enabled
// config always yields a verifier. If NewVerifier grows a second nil return,
// this fails and the enforcing-but-unverifiable case needs an explicit
// fail-closed leg.
func TestEnabledCaptchaAlwaysHasVerifier(t *testing.T) {
	for _, provider := range []string{"turnstile", "recaptcha-v3", "hcaptcha", "recaptcha", "not-a-provider", ""} {
		cfg := &config.CaptchaConfig{SiteKey: "site", SecretKey: "secret", Provider: provider}
		require.True(t, cfg.IsEnabled())
		require.NotNil(t, captcha.NewVerifier(cfg, nil), "provider %q", provider)
	}
	for _, cfg := range []*config.CaptchaConfig{nil, {}, {SiteKey: "site"}, {SecretKey: "secret"}, {SiteKey: " ", SecretKey: "secret"}} {
		require.False(t, cfg.IsEnabled())
		require.Nil(t, captcha.NewVerifier(cfg, nil))
		require.False(t, captcha.ShouldApply(cfg, "checkout"), "disabled captcha must never enforce")
	}
}
