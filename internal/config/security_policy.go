package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Webhook secret overlap bounds (SEC-29).
const (
	DefaultWebhookSecretOverlap = 24 * time.Hour
	MaxWebhookSecretOverlap     = 7 * 24 * time.Hour
)

// WebhookSecretOverlapDuration is how long a rotated-out webhook signing secret
// keeps verifying. Empty means 24h.
func (cfg *Config) WebhookSecretOverlapDuration() (time.Duration, error) {
	raw := ""
	if cfg != nil {
		raw = strings.TrimSpace(cfg.WebhookSecretOverlap)
	}
	if raw == "" {
		return DefaultWebhookSecretOverlap, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("webhook_secret_overlap %q is not a Go duration (e.g. 24h): %w", raw, err)
	}
	if d < time.Minute || d > MaxWebhookSecretOverlap {
		return 0, fmt.Errorf("webhook_secret_overlap must be between 1m and %s", MaxWebhookSecretOverlap)
	}
	return d, nil
}

// AllowedReturnOrigins are the exact origins checkout success/cancel and
// billing-portal return URLs may name (SEC-33): ReturnOrigins, else the origin
// of PublicBillingBaseURL. Empty refuses every return URL.
func (cfg *Config) AllowedReturnOrigins() []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, raw := range cfg.ReturnOrigins {
		if origin, ok := URLOrigin(raw); ok {
			out = append(out, origin)
		}
	}
	if len(out) == 0 {
		if origin, ok := URLOrigin(cfg.PublicBillingBaseURL); ok {
			out = append(out, origin)
		}
	}
	return out
}

// ReturnURLAllowed reports whether raw is an absolute URL whose origin exactly
// matches an allowed return origin.
func (cfg *Config) ReturnURLAllowed(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Opaque != "" {
		return false
	}
	origin, ok := URLOrigin(raw)
	if !ok {
		return false
	}
	for _, allowed := range cfg.AllowedReturnOrigins() {
		if origin == allowed {
			return true
		}
	}
	return false
}

// URLOrigin returns scheme://host[:port] in lower case for an absolute http(s) URL.
func URLOrigin(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), true
}

func validateSecurityPolicy(cfg *Config) error {
	if _, err := cfg.WebhookSecretOverlapDuration(); err != nil {
		return err
	}
	for _, raw := range cfg.ReturnOrigins {
		if err := validatePublicURL(strings.TrimSpace(raw), true, true); err != nil {
			return fmt.Errorf("return_origins %q %w", raw, err)
		}
	}
	return nil
}
