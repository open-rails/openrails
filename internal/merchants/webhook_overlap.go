package merchants

import (
	"time"

	"github.com/jonboulle/clockwork"
)

// SEC-29: a rotated-out webhook signing secret verifies only until an explicit
// expiry, the PSP's webhook_overlap_expires_at setting. Rotation records it; a
// declared previous secret must carry it too. No expiry, or an unparsable one,
// means the old secret is refused.
const WebhookOverlapExpiresKey = "webhook_overlap_expires_at"

// A rotated-out PSP webhook signing secret keeps verifying for 24 h, so
// callbacks signed before the rotation don't fail; retire_webhook_overlap
// ends it early.
const webhookSecretOverlap = 24 * time.Hour

// A managed Stripe endpoint's outgoing secret keeps verifying for a week.
const managedEndpointOverlap = 7 * 24 * time.Hour

// WithClock sets the clock that bounds webhook secret overlaps.
func (s *Service) WithClock(clock clockwork.Clock) *Service {
	if s != nil && clock != nil {
		s.clock = clock
	}
	return s
}

func (s *Service) now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now().UTC()
	}
	return s.clock.Now().UTC()
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
