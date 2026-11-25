package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/open-rails/openrails/billing"
)

// Degradation is one optional setting boot turned off, or set back to its
// default, because it cannot be served as written. The engine serves without
// it and reports it (see Runtime.Refusals); it never fails boot.
type Degradation struct {
	// Field names the setting: "captcha", "trusted_proxies", "hyperswitch", ...
	Field string
	Err   error
}

func (d Degradation) Error() string { return d.Field + ": " + d.Err.Error() }

// Degrade turns off each optional setting of cfg that would not validate, or
// sets it back to its default, and returns what it changed. A list setting
// keeps its valid entries, so a trust or allow list only ever shrinks. What
// Degrade leaves is what Validate requires: the database schema and the
// provider write mode.
func Degrade(cfg *billing.Config) []Degradation {
	if cfg == nil {
		return nil
	}
	var out []Degradation
	off := func(field string, err error) { out = append(out, Degradation{Field: field, Err: err}) }

	buckets, defaultBuckets := RateLimitBuckets(&cfg.RateLimits), RateLimitBuckets(&defaults.RateLimits)
	for _, bucket := range slices.Sorted(maps.Keys(buckets)) {
		if limit := *buckets[bucket]; limit != nil && limit.RequestsPerMinute < 0 {
			fallback := **defaultBuckets[bucket]
			*buckets[bucket] = &fallback
			off("rate_limits."+bucket+".requests_per_minute", fmt.Errorf("%d is negative (0 does not limit); the default %d applies", limit.RequestsPerMinute, (*defaultBuckets[bucket]).RequestsPerMinute))
		}
	}
	if err := validateCaptcha(cfg.Captcha); err != nil {
		cfg.Captcha = defaults.Captcha
		off("captcha", fmt.Errorf("%w; captcha is off", err))
	}
	if err := validateHyperSwitch(cfg); err != nil {
		cfg.HyperSwitch = billing.HyperSwitchConfig{}
		off("hyperswitch", fmt.Errorf("%w; HyperSwitch is off", err))
	}
	if crankers := SolanaCrankers(cfg); len(crankers) > 0 {
		var kept []billing.SolanaCrankerConfig
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
		cfg.Solana = billing.SolanaConfig{Crankers: kept}
	}
	if err := validateCardVault(cfg); err != nil {
		cfg.CardVault = billing.CardVaultConfig{}
		off("card_vault", fmt.Errorf("%w; settings.card_vault platform is refused", err))
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
	cfg.TrustedProxies = keep("trusted_proxies", cfg.TrustedProxies, validateSourceCIDR)
	return out
}
