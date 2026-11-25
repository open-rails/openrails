package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// MaxWebhookSecretOverlap bounds webhook_secret_overlap (SEC-29).
const MaxWebhookSecretOverlap = 7 * 24 * time.Hour

// WebhookSecretOverlapDuration is how long a rotated-out webhook signing secret
// keeps verifying: the configured overlap, the default when unset.
func WebhookSecretOverlapDuration(cfg *billing.Config) (time.Duration, error) {
	if cfg == nil || cfg.WebhookSecretOverlap == 0 {
		return defaults.WebhookSecretOverlap, nil
	}
	d := cfg.WebhookSecretOverlap
	if d < time.Minute || d > MaxWebhookSecretOverlap {
		return 0, fmt.Errorf("webhook_secret_overlap must be between 1m and %s", MaxWebhookSecretOverlap)
	}
	return d, nil
}

// AllowedReturnOrigins are the exact origins purchase success/cancel and
// billing-portal return URLs may name (SEC-33): ReturnOrigins, else the origin
// of PublicBillingBaseURL. Empty refuses every return URL.
func AllowedReturnOrigins(cfg *billing.Config) []string {
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
func ReturnURLAllowed(cfg *billing.Config, raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Opaque != "" {
		return false
	}
	origin, ok := URLOrigin(raw)
	if !ok {
		return false
	}
	for _, allowed := range AllowedReturnOrigins(cfg) {
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
