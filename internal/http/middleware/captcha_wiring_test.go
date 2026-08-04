package middleware

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/captcha"
)

// or#865 / FC-13. Captcha's fail-closed property (a captcha that does not
// verify is INVALID, never a pass) rests on the `err != nil` and
// `!result.Success` legs of evaluateCaptchaVerify, which are live and tested.
// It ALSO silently assumed deps.Verifier is never nil while enforcement is on.
// That assumption was previously "guarded" by a runtime branch that could not
// fire in any configuration; the assumption itself is what can actually break,
// so it is asserted here instead.
//
// If captcha.NewVerifier ever grows a second nil return (a bad provider name, a
// failed client build), this test fires and whoever added it must decide what
// an enforcing-but-unverifiable captcha should do — rather than discovering it
// as a nil dereference in the rate-limit middleware.
func TestEnabledCaptchaAlwaysHasVerifier(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.CaptchaConfig
	}{
		{"turnstile", &config.CaptchaConfig{SiteKey: "site", SecretKey: "secret", Provider: "turnstile"}},
		{"recaptcha", &config.CaptchaConfig{SiteKey: "site", SecretKey: "secret", Provider: "recaptcha"}},
		{"hcaptcha", &config.CaptchaConfig{SiteKey: "site", SecretKey: "secret", Provider: "hcaptcha"}},
		{"unknown provider", &config.CaptchaConfig{SiteKey: "site", SecretKey: "secret", Provider: "not-a-provider"}},
		{"empty provider defaults", &config.CaptchaConfig{SiteKey: "site", SecretKey: "secret"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.cfg.IsEnabled() {
				t.Fatalf("fixture is not enabled; the test no longer covers the enabled case")
			}
			if v := captcha.NewVerifier(tc.cfg, nil); v == nil {
				t.Fatal("captcha is enabled but NewVerifier returned nil: evaluateCaptchaVerify would nil-dereference. " +
					"Decide the policy for an enforcing-but-unverifiable captcha (it must FAIL CLOSED) and reinstate an explicit leg.")
			}
		})
	}
}

// The other half of the coupling: disabled captcha yields no verifier, and
// nothing may then reach evaluateCaptchaVerify.
func TestDisabledCaptchaHasNoVerifierAndDoesNotEnforce(t *testing.T) {
	for _, cfg := range []*config.CaptchaConfig{
		nil,
		{},
		{SiteKey: "site"},
		{SecretKey: "secret"},
	} {
		if cfg.IsEnabled() {
			t.Fatalf("fixture %+v unexpectedly enabled", cfg)
		}
		if v := captcha.NewVerifier(cfg, nil); v != nil {
			t.Fatalf("disabled captcha built a verifier: %T", v)
		}
		if captchaShouldEnforce(cfg, nil, "auth") {
			t.Fatal("disabled captcha must not enforce")
		}
	}
}
