package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Degradation is one optional setting boot turned off, or set back to its
// default, because it cannot be served as written. The engine serves without
// it and reports it (see Runtime.Refusals); it never fails boot.
type Degradation struct {
	// Field names the setting: "captcha", "trusted_proxies", "llm", ...
	Field string
	Err   error
}

func (d Degradation) Error() string { return d.Field + ": " + d.Err.Error() }

// Degrade turns off each optional setting of cfg that would not validate, or
// sets it back to its default, and returns what it changed. A list setting
// keeps its valid entries, so a trust or allow list only ever shrinks. What
// Degrade leaves is what Validate requires: the database and the provider
// write mode.
func Degrade(cfg *Config) []Degradation {
	if cfg == nil {
		return nil
	}
	var out []Degradation
	off := func(field string, err error) { out = append(out, Degradation{Field: field, Err: err}) }

	if _, err := EventRetentionDuration(cfg); err != nil {
		cfg.EventRetention = ""
		off("event_retention", fmt.Errorf("%w; the default 90 days applies", err))
	}
	if cfg.RateLimits != nil {
		limits := maps.Clone(*cfg.RateLimits)
		for _, bucket := range slices.Sorted(maps.Keys(limits)) {
			if !slices.Contains(RateLimitBuckets, bucket) {
				delete(limits, bucket)
				off("rate_limits."+bucket, fmt.Errorf("not a bucket (%s); a per-address ceiling belongs to the proxy in front of OpenRails; ignored", strings.Join(RateLimitBuckets, ", ")))
			}
		}
		cfg.RateLimits = &limits
	}
	if cfg.Redis != nil {
		if _, err := RedisOptions(cfg.Redis); err != nil {
			cfg.Redis = nil
			off("redis", fmt.Errorf("%w; abuse state is kept in this process's memory", err))
		}
	}
	if err := validateCaptcha(cfg.Captcha); err != nil {
		cfg.Captcha = DefaultCaptcha()
		off("captcha", fmt.Errorf("%w; captcha is off", err))
	}
	if err := validateLLM(cfg.LLM); err != nil {
		cfg.LLM = nil
		off("llm", fmt.Errorf("%w; the catalog copilot is off", err))
	}
	if _, _, err := CatalogReconciliationSchedule(cfg); err != nil {
		cfg.CatalogReconciliationInterval = ""
		off("catalog_reconciliation_interval", fmt.Errorf("%w; the default 1h applies", err))
	}
	if _, err := WebhookSecretOverlapDuration(cfg); err != nil {
		cfg.WebhookSecretOverlap = ""
		off("webhook_secret_overlap", fmt.Errorf("%w; the default %s applies", err, DefaultWebhookSecretOverlap))
	}
	if err := validateHyperSwitch(cfg); err != nil {
		cfg.HyperSwitch = nil
		off("hyperswitch", fmt.Errorf("%w; HyperSwitch is off", err))
	}
	if crankers := SolanaCrankers(cfg); len(crankers) > 0 {
		var kept []SolanaCrankerConfig
		for i, c := range crankers {
			if err := validateSolanaCranker(cfg, kept, c); err != nil {
				next := ""
				if i == 0 {
					next = "; the next listed cranker publishes new plans"
				}
				off(fmt.Sprintf("solana.crankers[%d]", i), fmt.Errorf("%w; it pulls none of its plans%s", err, next))
				continue
			}
			kept = append(kept, c)
		}
		cfg.Solana = &SolanaConfig{Crankers: kept}
	}
	if err := validateCardVault(cfg); err != nil {
		cfg.CardVault = nil
		off("card_vault", fmt.Errorf("%w; settings.card_vault platform is refused", err))
	}
	if s := cfg.ProviderSandbox; s != nil {
		sandbox := *s
		for _, gateway := range []struct {
			key string
			url *string
		}{{"nmi_gateway_url", &sandbox.NMIGatewayURL}, {"stripe_api_url", &sandbox.StripeAPIURL}, {"solana_rpc_url", &sandbox.SolanaRPCURL}, {"ccbill_datalink_url", &sandbox.CCBillDataLinkURL}} {
			if raw := strings.TrimSpace(*gateway.url); raw != "" {
				if err := ValidateLoopbackGatewayURL(raw); err != nil {
					*gateway.url = ""
					off("provider_sandbox."+gateway.key, fmt.Errorf("%w; the provider's own sandbox endpoint applies", err))
				}
			}
		}
		cfg.ProviderSandbox = &sandbox
	}
	keep := func(field string, list []string, check func(string) error) []string {
		var kept []string
		for _, raw := range list {
			if err := check(raw); err != nil {
				off(field, fmt.Errorf("%w; entry ignored", err))
				continue
			}
			kept = append(kept, raw)
		}
		return kept
	}
	cfg.ReturnOrigins = keep("return_origins", cfg.ReturnOrigins, func(raw string) error {
		if err := ValidatePublicURL(strings.TrimSpace(raw), true, true); err != nil {
			return fmt.Errorf("%q %w", raw, err)
		}
		return nil
	})
	cfg.TrustedProxies = keep("trusted_proxies", cfg.TrustedProxies, validateSourceCIDR)
	cfg.CloudflareProxies = keep("cloudflare_proxies", cfg.CloudflareProxies, validateSourceCIDR)
	cfg.CCBillWebhookIPAllowlist = keep("ccbill_webhook_ip_allowlist", cfg.CCBillWebhookIPAllowlist, validateSourceCIDR)
	return out
}

// DegradeEmail keeps one email sender: the host's when it lends one.
func DegradeEmail(cfg *Config, hostSender bool) []Degradation {
	if cfg == nil || cfg.SMTP == nil {
		return nil
	}
	switch {
	case hostSender:
		cfg.SMTP = nil
		return []Degradation{{Field: "smtp", Err: errors.New("Deps.Email sends OpenRails' email; Config.SMTP is ignored")}}
	case strings.TrimSpace(cfg.SMTP.Host) == "":
		cfg.SMTP = nil
		return []Degradation{{Field: "smtp", Err: errors.New("Config.SMTP.Host is required; OpenRails sends no email")}}
	}
	return nil
}
