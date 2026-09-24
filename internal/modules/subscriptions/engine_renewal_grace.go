package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/entitlements"
)

// Engine renewal grace keeps an engine member's access across the paid-period
// boundary until the engine's own renewal decides. A qualified renewal
// supersedes it; a decline, cancellation or terminal outcome revokes it. If no
// outcome ever arrives (the fleet is down), access still ends when it expires.
//
// The grace is a bounded share of the period, never a fixed day (a fixed 24h
// gave an hourly member 24 free periods):
//
//	grace(period) = min(24h, max(5m, period/10))
//
// period/10 caps unpaid access at 10% of what was paid for; 24h caps it for
// long periods (from 10 days up, a day is ample for a renewal to settle); 5m is
// five due passes (the due pass runs every minute), the least time a renewal
// needs to be admitted and charged. Periods are whole hours, so the floor
// never binds and the free fraction is at most 10% for every cadence:
// 1h -> 6m, 1d -> 2h24m, 7d -> 16h48m, 30d/90d/365d -> 24h.
const (
	engineGraceCap      = 24 * time.Hour
	engineGraceFloor    = 5 * time.Minute
	engineGraceFraction = 10
)

// EngineRenewalGrace returns grace(period); an unknown period fails closed.
func EngineRenewalGrace(period time.Duration) (time.Duration, error) {
	if period <= 0 {
		return 0, &collection.UnknownCycleError{CycleHours: int(period / time.Hour)}
	}
	return min(engineGraceCap, max(engineGraceFloor, period/engineGraceFraction)), nil
}

// EngineAuthenticationWindow bounds how long a first payment may wait on an
// issuer authentication challenge the payer started. After it the engine
// closes the payment and the enrollment fails, releasing the product for a
// new attempt. A renewal's challenge waits at most EngineRenewalGrace of the
// renewal's period, the same bound as its access.
const EngineAuthenticationWindow = time.Hour

type graceWriter interface {
	PushNewEntitlement(context.Context, entitlements.PushNewEntitlementParams) (*models.Entitlement, error)
}

// pushEngineRenewalGrace appends the renewal allowance after periodEnd for a
// live engine-owned subscription. Other collection policies project their own
// access (provider cohorts hold standing windows).
func pushEngineRenewalGrace(ctx context.Context, ent graceWriter, sub *models.Subscription, names []string, periodStart, periodEnd time.Time) error {
	if ent == nil || sub == nil || sub.CollectionPolicy != models.CollectionPolicyEngine || sub.CancelledAt != nil || sub.Status == models.StatusCancelled {
		return nil
	}
	grace, err := EngineRenewalGrace(periodEnd.Sub(periodStart))
	if err != nil {
		return fmt.Errorf("engine renewal grace for %s: %w", sub.ID, err)
	}
	start := periodEnd.UTC()
	end := start.Add(grace)
	for _, name := range names {
		if _, err := ent.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{UserID: sub.CustomerID.String(), Entitlement: name, NotBefore: &start, EndAt: &end, SourceType: models.EntitlementSourceGrace, SourceID: sub.ID}); err != nil {
			return fmt.Errorf("grant engine renewal grace %s: %w", name, err)
		}
	}
	return nil
}

func entitlementNames(spec map[string]*int) []string {
	names := make([]string, 0, len(spec))
	for name := range spec {
		names = append(names, name)
	}
	return names
}
