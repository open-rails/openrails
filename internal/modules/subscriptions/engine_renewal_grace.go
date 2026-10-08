package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/entitlements"
)

// Engine renewal grace keeps an engine member's access across the paid-period
// boundary until the engine's own renewal decides. A qualified renewal
// supersedes it; a decline, cancellation or terminal outcome revokes it.
//
// A renewal with no outcome past its allowance is held: collection is stopped
// (fleet halted, admission hold, breaker, readonly). By default the member
// keeps access until the renewal is attempted, and life.renewal.held reports
// the backlog; access_while_renewal_held=suspend ends access at the allowance.
//
// The allowance is a bounded share of the period, never a fixed day (a fixed
// 24h gave an hourly member 24 free periods):
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

// pushRenewalGrace appends explicit grace after matched paid access and billing
// boundaries: open-ended by default, bounded when the merchant suspends held
// renewals. Paid grants remain finite for both engine and provider schedules.
func pushRenewalGrace(ctx context.Context, d *db.DB, ent graceWriter, sub *models.Subscription, names []string, periodStart, periodEnd time.Time) error {
	if ent == nil || sub == nil || sub.CanceledAt != nil || !lifecycle.Status(sub.Status).Live() || !accessMatchesBillingPeriod(sub, periodStart, periodEnd) {
		return nil
	}
	grace, err := EngineRenewalGrace(time.Duration(*sub.AccessDurationHoursSnapshot) * time.Hour)
	if err != nil {
		return fmt.Errorf("renewal grace for %s: %w", sub.ID, err)
	}
	policy, err := CasePolicy(ctx, d, sub)
	if err != nil {
		return err
	}
	start := *accessEnd(periodStart, sub.AccessDurationHoursSnapshot)
	end := start.Add(grace)
	for _, name := range names {
		p := entitlements.PushNewEntitlementParams{UserID: sub.CustomerID.String(), Entitlement: name, NotBefore: &start, EndsAt: &end, SourceType: models.EntitlementSourceGrace, SourceID: sub.ID}
		if !policy.SuspendWhenHeld {
			p.EndsAt, p.Indefinite = nil, true
		}
		if _, err := ent.PushNewEntitlement(ctx, p); err != nil {
			return fmt.Errorf("grant renewal grace %s: %w", name, err)
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

// EnsureRenewalGrace materializes the merchant's renewal-hold policy for an
// existing paid subscription. The caller owns its transaction and row lock;
// imports use the same path as observed provider renewals.
func (s *SubscriptionLifecycleService) EnsureRenewalGrace(ctx context.Context, d *db.DB, sub *models.Subscription) error {
	if sub.CurrentPeriodStartsAt == nil || sub.CurrentPeriodEndsAt == nil {
		return nil
	}
	if sub.Status == models.StatusPastDue || sub.Status == models.StatusAwaitingMethod {
		return s.dunningAccess(ctx, d, s.newLifecycleEntitlementService(d), sub, s.now())
	}
	return pushRenewalGrace(ctx, d, s.newLifecycleEntitlementService(d), sub, entitlementNames(sub.EntitlementsSpecSnapshot), *sub.CurrentPeriodStartsAt, *sub.CurrentPeriodEndsAt)
}
