package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

// A deliberate cancellation (user, merchant, chargeback) is terminal: no
// renewal or reactivation may silently revive it without an explicit override.
// Only the typed cancel_type counts, never free-text feedback.
func TestTerminalCancellationBlocksReactivation(t *testing.T) {
	svc := &SubscriptionLifecycleService{}
	feedback := "CHARGEBACK: unauthorized"
	for _, tc := range []struct {
		name     string
		sub      *models.Subscription
		override bool
		blocked  bool
	}{
		{"chargeback", sub(models.RailCCBill, models.StatusCancelled, cancelType(models.CancelTypeChargeback)), false, true},
		{"user", sub(models.RailStripe, models.StatusCancelled, cancelType(models.CancelTypeUser)), false, true},
		{"merchant", sub(models.RailStripe, models.StatusCancelled, cancelType(models.CancelTypeMerchant)), false, true},
		{"chargeback with override", sub(models.RailCCBill, models.StatusCancelled, cancelType(models.CancelTypeChargeback)), true, false},
		{"expired", sub(models.RailCCBill, models.StatusCancelled, cancelType(models.CancelTypeExpired)), false, false},
		{"no cancel type", sub(models.RailCCBill, models.StatusCancelled), false, false},
		{"chargeback feedback only", sub(models.RailCCBill, models.StatusCancelled, func(s *models.Subscription) { s.CancelFeedback = &feedback }), false, false},
		{"not cancelled", sub(models.RailStripe, models.StatusPastDue, cancelType(models.CancelTypeUser)), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.assertActiveTransitionAllowed(context.Background(), tc.sub, "renewal", tc.override)
			require.Equal(t, tc.blocked, err != nil, "%v", err)
			if tc.blocked {
				require.True(t, IsTerminalTransitionBlocked(err))
			}
		})
	}

	past := time.Now().UTC().Add(-time.Hour)
	for _, end := range []*time.Time{nil, {}, &past} {
		_, err := svc.ReactivateMembership(context.Background(), &ReactivateMembershipParams{Rail: models.RailCCBill, RailSubscriptionID: "sub_123", CurrentPeriodEndsAt: end})
		require.ErrorContains(t, err, "future paid-through period end")
	}
}

// An on-time or late-within-period renewal keeps the ordinary next period; once
// a whole period is missed, the admitted attempt buys one period from admission.
func TestSelectEngineRenewalPeriod(t *testing.T) {
	oldEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cycle := 30 * 24 * time.Hour
	accepted := RenewalTerms{PSPID: uuid.New(), SubscriptionID: uuid.New(), CustomerID: uuid.New(), FromPriceID: uuid.New(), FromProductID: uuid.New(), PriceID: uuid.New(), ProductID: uuid.New(), Amount: 10_000_000, Currency: "USD", PeriodStart: oldEnd, PeriodEnd: oldEnd.Add(cycle)}
	for _, tc := range []struct {
		name      string
		now, want time.Time
	}{
		{"on time", oldEnd, oldEnd},
		{"late within next period", oldEnd.Add(cycle - time.Second), oldEnd},
		{"whole period missed", oldEnd.Add(cycle), oldEnd.Add(cycle)},
		{"several periods missed", oldEnd.Add(100 * 24 * time.Hour), oldEnd.Add(100 * 24 * time.Hour)},
		{"sub-microsecond admission truncates", oldEnd.Add(cycle + 1500), oldEnd.Add(cycle + time.Microsecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			terms, err := SelectEngineRenewalPeriod(accepted, tc.now)
			require.NoError(t, err)
			require.Equal(t, tc.want, terms.PeriodStart)
			require.Equal(t, cycle, terms.PeriodEnd.Sub(terms.PeriodStart))
			require.Equal(t, accepted.Amount, terms.Amount)
		})
	}
	_, err := SelectEngineRenewalPeriod(accepted, oldEnd.Add(-time.Second))
	require.Error(t, err, "not yet due")
	_, err = SelectEngineRenewalPeriod(accepted, time.Time{})
	require.Error(t, err)
	require.Equal(t, oldEnd, accepted.PeriodStart, "selection never mutates the accepted terms")

	// Each attempt is fenced by the old boundary and its own attempt number.
	id := uuid.New()
	require.NotEqual(t, SubscriptionCollectionKey(id, oldEnd, 1), SubscriptionCollectionKey(id, oldEnd, 2))
	require.Equal(t, SubscriptionCollectionKey(id, oldEnd, 1), SubscriptionCollectionKey(id, oldEnd.In(time.FixedZone("x", 3600)), 1))
}

// Eligibility is computed against the service clock at read time and frozen in
// the response snapshot until the next read.
func TestUserSubscriptionResponseUsesReadClock(t *testing.T) {
	now := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(now)
	svc := &UserSubscriptionService{}
	svc.SetClock(clock)
	resp := &UserSubscriptionResponse{Subscription: sub(models.RailStripe, models.StatusCancelled, endsAt(now.Add(time.Hour)))}
	check := func(want bool) {
		t.Helper()
		view := resp.View()
		require.Equal(t, want, view.Resumable)
		require.Equal(t, want, view.CancelScheduled)
	}
	require.NoError(t, svc.enrichSubscriptionResponses(context.Background(), []*UserSubscriptionResponse{resp}))
	check(true)
	clock.Advance(2 * time.Hour)
	check(true)
	require.NoError(t, svc.enrichSubscriptionResponses(context.Background(), []*UserSubscriptionResponse{resp}))
	check(false)
}
