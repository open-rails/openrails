package merchants

import (
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/config"
)

// SEC-29: a rotated-out webhook signing secret verifies only until an explicit
// expiry, the PSP's webhook_overlap_expires_at setting. Rotation records it; a
// declared previous secret must carry it too. No expiry, or an unparsable one,
// means the old secret is refused.
const WebhookOverlapExpiresKey = "webhook_overlap_expires_at"

// WithClock sets the clock that bounds webhook secret overlaps.
func (s *Service) WithClock(clock clockwork.Clock) *Service {
	if s != nil && clock != nil {
		s.clock = clock
	}
	return s
}

// WithWebhookSecretOverlap sets how long a rotated-out webhook secret keeps
// verifying. Zero keeps the default; values are capped at config.MaxWebhookSecretOverlap.
func (s *Service) WithWebhookSecretOverlap(d time.Duration) *Service {
	if s != nil && d > 0 {
		s.webhookSecretOverlap = min(d, config.MaxWebhookSecretOverlap)
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
		return config.DefaultWebhookSecretOverlap
	}
	return s.webhookSecretOverlap
}

// overlapOpen reports whether the PSP's rotated-out webhook secret still
// verifies.
func (s *Service) overlapOpen(scope PSPScope) bool {
	return !scope.WebhookOverlapUntil.IsZero() && s.now().Before(scope.WebhookOverlapUntil)
}

// hasWebhookOverlap reports whether the rail keeps a previous webhook secret.
func hasWebhookOverlap(rail string) bool {
	return rail == "stripe" || rail == "nmi"
}
