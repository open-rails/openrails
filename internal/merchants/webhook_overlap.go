package merchants

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
)

// SEC-29: a rotated-out webhook signing secret verifies only until an explicit
// expiry. Rotation records it; a declared (snapshot) previous secret must carry
// the same setting. No expiry, or an unparsable one, means the old secret is
// refused.
const (
	DefaultWebhookSecretOverlap = 24 * time.Hour
	MaxWebhookSecretOverlap     = 7 * 24 * time.Hour
	WebhookOverlapExpiresKey    = "webhook_overlap_expires_at"
)

// WithClock sets the clock that bounds webhook secret overlaps.
func (s *Service) WithClock(clock clockwork.Clock) *Service {
	if s != nil && clock != nil {
		s.clock = clock
	}
	return s
}

// WithWebhookSecretOverlap sets how long a rotated-out webhook secret keeps
// verifying. Zero keeps the default; values are capped at MaxWebhookSecretOverlap.
func (s *Service) WithWebhookSecretOverlap(d time.Duration) *Service {
	if s != nil && d > 0 {
		s.webhookSecretOverlap = min(d, MaxWebhookSecretOverlap)
	}
	return s
}

func (s *Service) now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now().UTC()
	}
	return s.clock.Now().UTC()
}

func (s *Service) overlapWindow() time.Duration {
	if s == nil || s.webhookSecretOverlap <= 0 {
		return DefaultWebhookSecretOverlap
	}
	return s.webhookSecretOverlap
}

// webhookOverlapExpiry reads the published expiry, else the declared setting.
func webhookOverlapExpiry(evidence []byte) time.Time {
	var doc struct {
		ExpiresAt string         `json:"webhook_overlap_expires_at"`
		Settings  map[string]any `json:"settings"`
	}
	if len(evidence) == 0 || json.Unmarshal(evidence, &doc) != nil {
		return time.Time{}
	}
	raw := doc.ExpiresAt
	if raw == "" {
		raw, _ = doc.Settings[WebhookOverlapExpiresKey].(string)
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

// previousWebhookRef returns the previous secret's reference only while its
// overlap is open.
func (s *Service) previousWebhookRef(scope pspSecretScope) (SecretRef, error) {
	if scope.webhookOverlapUntil.IsZero() || !s.now().Before(scope.webhookOverlapUntil) {
		return SecretRef{}, nil
	}
	return scope.secretRef("webhook_signing_secret_previous")
}

// hasWebhookOverlap reports whether the rail keeps a previous webhook secret.
func hasWebhookOverlap(rail string) bool {
	return rail == "stripe" || rail == "nmi"
}
