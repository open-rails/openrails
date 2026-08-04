package subscriptions

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

func ptrTime(t time.Time) *time.Time { return &t }

func baseSub(rail models.Rail, status models.SubscriptionStatus) *models.Subscription {
	return &models.Subscription{
		ID:         uuid.New(),
		CustomerID: identity.CustomerIDFromString("user_1").UUID(),
		Rail:       rail,
		Status:     status,
	}
}

// TestCancelMode_PerRail locks the capability mapping.
func TestCancelMode_PerRail(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	future := now.Add(10 * 24 * time.Hour)

	t.Run("stripe is reversible", func(t *testing.T) {
		sub := baseSub(models.RailStripe, models.StatusActive)
		require.Equal(t, CancelModeReversible, CancelModeFor(sub, now))
	})

	t.Run("ccbill is destructive (#696 DataLink SMS cancel, no resume)", func(t *testing.T) {
		sub := baseSub(models.RailCCBill, models.StatusActive)
		require.Equal(t, CancelModeDestructive, CancelModeFor(sub, now))
	})

	t.Run("solana is destructive", func(t *testing.T) {
		sub := baseSub(models.RailSolana, models.StatusActive)
		require.Equal(t, CancelModeDestructive, CancelModeFor(sub, now))
	})

	t.Run("nmi/mobius destructive when no deferred delete", func(t *testing.T) {
		sub := baseSub(models.RailNMI, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		require.Equal(t, CancelModeDestructive, CancelModeFor(sub, now))
	})

	t.Run("nmi/mobius reversible only while deferred delete pending in-window", func(t *testing.T) {
		sub := baseSub(models.RailNMI, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		sub.DeletionScheduledAt = ptrTime(future.Add(-48 * time.Hour))
		require.Equal(t, CancelModeReversible, CancelModeFor(sub, now))
	})

	t.Run("nmi/mobius destructive once delete executed (DeletionScheduledAt cleared)", func(t *testing.T) {
		sub := baseSub(models.RailNMI, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		sub.DeletionScheduledAt = nil
		require.Equal(t, CancelModeDestructive, CancelModeFor(sub, now))
	})

	t.Run("nil subscription is destructive", func(t *testing.T) {
		require.Equal(t, CancelModeDestructive, CancelModeFor(nil, now))
	})
}

// TestResumable covers the shared predicate across the matrix the issue calls out.
func TestResumable(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	future := now.Add(10 * 24 * time.Hour)
	past := now.Add(-1 * time.Hour)

	t.Run("stripe cancelled mid-period is resumable", func(t *testing.T) {
		sub := baseSub(models.RailStripe, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		require.True(t, Resumable(sub, now))
		require.True(t, CancelScheduled(sub, now))
	})

	t.Run("stripe active is NOT resumable", func(t *testing.T) {
		sub := baseSub(models.RailStripe, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		require.False(t, Resumable(sub, now))
		require.False(t, CancelScheduled(sub, now))
	})

	t.Run("expired stripe cancelled is NOT resumable", func(t *testing.T) {
		sub := baseSub(models.RailStripe, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(past)
		require.False(t, Resumable(sub, now))
		require.False(t, CancelScheduled(sub, now))
	})

	t.Run("ccbill cancelled is NOT resumable", func(t *testing.T) {
		sub := baseSub(models.RailCCBill, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		require.False(t, Resumable(sub, now))
		require.Equal(t, CancelModeDestructive, CancelModeFor(sub, now))
	})

	t.Run("nmi cancelled without deferred delete is NOT resumable", func(t *testing.T) {
		sub := baseSub(models.RailNMI, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		sub.DeletionScheduledAt = nil
		require.False(t, Resumable(sub, now))
	})

	t.Run("nmi cancelled with deferred delete in-window IS resumable", func(t *testing.T) {
		sub := baseSub(models.RailNMI, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		sub.DeletionScheduledAt = ptrTime(future.Add(-48 * time.Hour))
		require.True(t, Resumable(sub, now))
	})

	t.Run("nil subscription is NOT resumable", func(t *testing.T) {
		require.False(t, Resumable(nil, now))
	})
}

func TestCancelPortalURL(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	// #696: no rail is external_portal anymore — CCBill cancels on OUR site.
	ccbill := baseSub(models.RailCCBill, models.StatusCancelled)
	require.Nil(t, CancelPortalURL(ccbill, now))

	stripe := baseSub(models.RailStripe, models.StatusCancelled)
	require.Nil(t, CancelPortalURL(stripe, now))
}

// TestResumableMatchesHandlerWorkerPrecondition locks the invariant that the
// DTO's Resumable equals the precondition the HTTP handler and River worker gate
// on. If anyone reintroduces a divergent inline check, this fails.
//
// The handler/worker precondition is: cancel mode reversible && status==cancelled
// && current_period_ends_at > now. We assert Resumable equals exactly that
// expression for a representative matrix.
func TestResumableMatchesHandlerWorkerPrecondition(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	future := now.Add(10 * 24 * time.Hour)
	past := now.Add(-1 * time.Hour)

	cases := []*models.Subscription{
		func() *models.Subscription {
			s := baseSub(models.RailStripe, models.StatusCancelled)
			s.CurrentPeriodEndsAt = ptrTime(future)
			return s
		}(),
		func() *models.Subscription {
			s := baseSub(models.RailStripe, models.StatusActive)
			s.CurrentPeriodEndsAt = ptrTime(future)
			return s
		}(),
		func() *models.Subscription {
			s := baseSub(models.RailStripe, models.StatusCancelled)
			s.CurrentPeriodEndsAt = ptrTime(past)
			return s
		}(),
		func() *models.Subscription {
			s := baseSub(models.RailCCBill, models.StatusCancelled)
			s.CurrentPeriodEndsAt = ptrTime(future)
			return s
		}(),
		func() *models.Subscription {
			s := baseSub(models.RailNMI, models.StatusCancelled)
			s.CurrentPeriodEndsAt = ptrTime(future)
			s.DeletionScheduledAt = ptrTime(future.Add(-48 * time.Hour))
			return s
		}(),
		func() *models.Subscription {
			s := baseSub(models.RailNMI, models.StatusCancelled)
			s.CurrentPeriodEndsAt = ptrTime(future)
			return s
		}(),
	}

	for i, sub := range cases {
		precondition := CancelModeFor(sub, now) == CancelModeReversible &&
			sub.Status == models.StatusCancelled &&
			sub.CurrentPeriodEndsAt != nil &&
			!sub.CurrentPeriodEndsAt.IsZero() &&
			sub.CurrentPeriodEndsAt.After(now)
		require.Equalf(t, precondition, Resumable(sub, now), "case %d: Resumable must equal handler/worker precondition", i)
	}
}

// TestNMIDeferredDeleteAt covers the defer-vs-immediate decision (issue 216).
func TestNMIDeferredDeleteAt(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	t.Run("cancel >48h before rebill defers at period_end-48h", func(t *testing.T) {
		periodEnd := now.Add(10 * 24 * time.Hour)
		sub := baseSub(models.RailNMI, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(periodEnd)

		deleteAt, defer_ := NMIDeferredDeleteAt(sub, now)
		require.True(t, defer_)
		require.Equal(t, periodEnd.Add(-NMIDeleteSafetyMargin), deleteAt)
		require.True(t, deleteAt.After(now))
	})

	t.Run("cancel exactly at the 48h boundary deletes immediately", func(t *testing.T) {
		// period_end - 48h == now  => deleteAt is not strictly after now => immediate.
		periodEnd := now.Add(NMIDeleteSafetyMargin)
		sub := baseSub(models.RailNMI, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(periodEnd)

		_, defer_ := NMIDeferredDeleteAt(sub, now)
		require.False(t, defer_)
	})

	t.Run("cancel within 48h of rebill deletes immediately", func(t *testing.T) {
		periodEnd := now.Add(12 * time.Hour) // well inside the 48h window
		sub := baseSub(models.RailNMI, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(periodEnd)

		_, defer_ := NMIDeferredDeleteAt(sub, now)
		require.False(t, defer_)
	})

	t.Run("unknown period end deletes immediately", func(t *testing.T) {
		sub := baseSub(models.RailNMI, models.StatusActive)
		sub.CurrentPeriodEndsAt = nil
		_, defer_ := NMIDeferredDeleteAt(sub, now)
		require.False(t, defer_)
	})

	t.Run("past period end deletes immediately", func(t *testing.T) {
		sub := baseSub(models.RailNMI, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(now.Add(-1 * time.Hour))
		_, defer_ := NMIDeferredDeleteAt(sub, now)
		require.False(t, defer_)
	})

	t.Run("nil subscription deletes immediately", func(t *testing.T) {
		_, defer_ := NMIDeferredDeleteAt(nil, now)
		require.False(t, defer_)
	})
}

// TestNMIResumeWindowLifecycle locks the resumability transitions across the
// deferred-delete lifecycle (issue 216).
func TestNMIResumeWindowLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	periodEnd := now.Add(10 * 24 * time.Hour)
	deleteAt := periodEnd.Add(-NMIDeleteSafetyMargin)

	// 1. Cancelled with deferred delete pending, before deadline: reversible & resumable.
	sub := baseSub(models.RailNMI, models.StatusCancelled)
	sub.CurrentPeriodEndsAt = ptrTime(periodEnd)
	sub.DeletionScheduledAt = ptrTime(deleteAt)
	require.Equal(t, CancelModeReversible, CancelModeFor(sub, now))
	require.True(t, Resumable(sub, now))

	// 2. After the delete fires (finalizer clears DeletionScheduledAt): destructive, not resumable.
	sub.DeletionScheduledAt = nil
	require.Equal(t, CancelModeDestructive, CancelModeFor(sub, now))
	require.False(t, Resumable(sub, now))

	// 3. Past the paid period (even if somehow still scheduled): not resumable.
	sub.DeletionScheduledAt = ptrTime(deleteAt)
	sub.CurrentPeriodEndsAt = ptrTime(now.Add(-1 * time.Hour))
	require.False(t, Resumable(sub, now))
}

// or#842: the AUTOMATED counterpart. A user cancel gets an undo window; the
// automated terminal cancels (dunning exhaustion, unknown-resolution
// convergence) queued the irreversible rail delete at `now`.
func TestSystemDeferredDeleteAt(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	t.Run("lapsed period gets the full cooling-off window", func(t *testing.T) {
		// The dominant automated shape: dunning exhausted on a period that ended
		// weeks ago. No known future charge date, so nothing shortens the window.
		sub := baseSub(models.RailNMI, models.StatusPastDue)
		sub.CurrentPeriodEndsAt = ptrTime(now.Add(-30 * 24 * time.Hour))
		require.Equal(t, now.Add(SystemDeleteCoolingOff), SystemDeferredDeleteAt(sub, now))
	})

	t.Run("unknown period end gets the full cooling-off window", func(t *testing.T) {
		sub := baseSub(models.RailNMI, models.StatusPastDue)
		sub.CurrentPeriodEndsAt = nil
		require.Equal(t, now.Add(SystemDeleteCoolingOff), SystemDeferredDeleteAt(sub, now))
	})

	t.Run("distant rebill: the window closes long before it", func(t *testing.T) {
		sub := baseSub(models.RailNMI, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(now.Add(10 * 24 * time.Hour))
		require.Equal(t, now.Add(SystemDeleteCoolingOff), SystemDeferredDeleteAt(sub, now))
	})

	t.Run("rebill inside the window shortens it to the safety margin", func(t *testing.T) {
		// period_end - 48h still in the future but sooner than now+24h.
		periodEnd := now.Add(NMIDeleteSafetyMargin + 6*time.Hour)
		sub := baseSub(models.RailNMI, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(periodEnd)
		require.Equal(t, periodEnd.Add(-NMIDeleteSafetyMargin), SystemDeferredDeleteAt(sub, now))
	})

	t.Run("no safe room at all is due immediately", func(t *testing.T) {
		// The pre-rebill margin has already opened: waiting would let the rail
		// bill a customer we just cancelled. Same answer as the user path.
		sub := baseSub(models.RailNMI, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(now.Add(12 * time.Hour))
		require.Equal(t, now, SystemDeferredDeleteAt(sub, now))
	})
}
