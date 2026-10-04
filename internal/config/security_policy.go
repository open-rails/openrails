package config

import (
	"fmt"
	"net"
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
func WebhookSecretOverlapDuration(cfg *Config) (time.Duration, error) {
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
// of PublicBillingBaseURL, then HTTP.Checkout.EmbedOrigins. Empty refuses
// every return URL.
func AllowedReturnOrigins(cfg *Config) []string {
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
	// The sites framing this host's payment page are where its redirect
	// rails return the buyer.
	for _, raw := range PublishedCheckout(cfg).EmbedOrigins {
		if origin, ok := URLOrigin(raw); ok {
			out = append(out, origin)
		}
	}
	return out
}

// ReturnURLAllowed reports whether raw is an absolute URL whose origin exactly
// matches an allowed return origin.
func ReturnURLAllowed(cfg *Config, raw string) bool {
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

// PublishedCheckout is Config.HTTP.Checkout, zero when checkout is not published.
func PublishedCheckout(cfg *Config) CheckoutConfig {
	if cfg == nil || cfg.HTTP == nil || cfg.HTTP.Checkout == nil {
		return CheckoutConfig{}
	}
	return *cfg.HTTP.Checkout
}

// CheckoutEmbedAllowed reports whether origin may frame the payment page this
// host serves: one of EmbedOrigins, or the page's own origin.
func CheckoutEmbedAllowed(cfg *Config, origin string) bool {
	if origin == "" {
		return false
	}
	checkout := PublishedCheckout(cfg)
	if page, ok := URLOrigin(checkout.PageURL); ok && page == origin {
		return true
	}
	for _, raw := range checkout.EmbedOrigins {
		if allowed, ok := URLOrigin(raw); ok && allowed == origin {
			return true
		}
	}
	return false
}

// CheckoutFrameAncestors is the Content-Security-Policy of the payment page
// this host serves: only the host itself and EmbedOrigins may frame it.
func CheckoutFrameAncestors(cfg *Config) string {
	policy := "frame-ancestors 'self'"
	for _, raw := range PublishedCheckout(cfg).EmbedOrigins {
		if origin, ok := URLOrigin(raw); ok {
			policy += " " + origin
		}
	}
	return policy
}

// ValidateCheckout checks PageURL and EmbedOrigins.
func ValidateCheckout(c CheckoutConfig) error {
	if raw := strings.TrimSpace(c.PageURL); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || !webScheme(u) {
			return fmt.Errorf("PageURL %q must be an absolute https URL (http on loopback) without credentials or a fragment", c.PageURL)
		}
	}
	for _, raw := range c.EmbedOrigins {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || u.User != nil || !webScheme(u) || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("EmbedOrigins %q must be an origin: https://host[:port] (http on loopback)", raw)
		}
	}
	return nil
}

// webScheme admits https, and http for a loopback host.
func webScheme(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	return u.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback())
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
	if _, err := WebhookSecretOverlapDuration(cfg); err != nil {
		return err
	}
	for _, raw := range cfg.ReturnOrigins {
		if err := validatePublicURL(strings.TrimSpace(raw), true, true); err != nil {
			return fmt.Errorf("return_origins %q %w", raw, err)
		}
	}
	return nil
}
