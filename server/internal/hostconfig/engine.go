package hostconfig

import (
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/config"
)

// EngineConfig is the engine's configuration as loaded, as openrails.New
// takes it. Each settings type converts as a whole, so a field one side
// lacks fails to compile. ReadCache has no key: the defaults serve.
func EngineConfig(c *config.Config) openrails.Config {
	out := openrails.Config{
		ProviderWriteMode:       c.ProviderWriteMode,
		EngineAdmissionHold:     c.EngineAdmissionHold,
		UsageBillingEnabled:     c.UsageBillingEnabled,
		Database:                openrails.DatabaseConfig(c.Database),
		Merchants:               c.Merchants,
		PublicBillingBaseURL:    c.PublicBillingBaseURL,
		ReturnOrigins:           c.ReturnOrigins,
		TrustedProxies:          c.TrustedProxies,
		CheckoutOrigin:          c.CheckoutOrigin,
		Captcha:                 convert(c.Captcha, func(v config.CaptchaConfig) openrails.CaptchaConfig { return openrails.CaptchaConfig(v) }),
		UserActionRateLimit:     openrails.UserActionRateLimitConfig(c.UserActionRateLimit),
		DestructiveActionLimits: openrails.DestructiveActionLimitsConfig(c.DestructiveActionLimits),
		Vault:                   convert(c.Vault, func(v config.VaultConfig) openrails.VaultConfig { return openrails.VaultConfig(v) }),
		CardVault:               convert(c.CardVault, func(v config.CardVaultConfig) openrails.CardVaultConfig { return openrails.CardVaultConfig(v) }),
		HyperSwitch:             convert(c.HyperSwitch, func(v config.HyperSwitchConfig) openrails.HyperSwitchConfig { return openrails.HyperSwitchConfig(v) }),
		LLM:                     convert(c.LLM, func(v config.LLMConfig) openrails.LLMConfig { return openrails.LLMConfig(v) }),
		ProviderSandbox: convert(c.ProviderSandbox, func(v config.ProviderSandboxConfig) openrails.ProviderSandboxConfig {
			return openrails.ProviderSandboxConfig(v)
		}),
		EventRetention:       c.EventRetention,
		WebhookSecretOverlap: c.WebhookSecretOverlap,
	}
	if c.RateLimits != nil {
		out.RateLimits = make(openrails.RateLimitsConfig, len(c.RateLimits))
		for bucket, limit := range c.RateLimits {
			out.RateLimits[bucket] = convert(limit, func(v config.RateLimit) openrails.RateLimit { return openrails.RateLimit(v) })
		}
	}
	out.Solana = convert(c.Solana, func(s config.SolanaConfig) openrails.SolanaConfig {
		crankers := make([]openrails.SolanaCrankerConfig, len(s.Crankers))
		for i, v := range s.Crankers {
			crankers[i] = openrails.SolanaCrankerConfig(v)
		}
		return openrails.SolanaConfig{Crankers: crankers}
	})
	return out
}

// convert is f of *p, or nil for nil.
func convert[U, T any](p *T, f func(T) U) *U {
	if p == nil {
		return nil
	}
	u := f(*p)
	return &u
}
