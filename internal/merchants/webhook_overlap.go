package merchants

import (
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/shared/apperr"
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

// webhookSecretKey is the rail's webhook secret slot, which rotation keeps
// verifying as its _previous slot; "" when the rail keeps none.
func webhookSecretKey(rail string) string {
	switch rail {
	case "stripe", "nmi":
		return "webhook_signing_secret"
	case "ccbill":
		return "webhook_token"
	}
	return ""
}

// suppliedPrevious refuses a _previous webhook secret: rotation retains it.
func suppliedPrevious(credentials map[string]string) error {
	for _, key := range []string{"webhook_signing_secret_previous", "webhook_token_previous"} {
		if _, supplied := credentials[key]; supplied {
			return apperr.Invalidf("%s is retained by rotation and cannot be supplied", key).WithParam("credentials")
		}
	}
	return nil
}
