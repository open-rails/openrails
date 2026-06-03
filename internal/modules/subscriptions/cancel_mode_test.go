package subscriptions

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func ptrTime(t time.Time) *time.Time { return &t }

func baseSub(processor models.Processor, status models.SubscriptionStatus) *models.Subscription {
	return &models.Subscription{
		ID:        uuid.New(),
		UserID:    "user_1",
		Processor: processor,
		Status:    status,
	}
}

// TestCancelMode_PerProcessor locks the capability mapping.
func TestCancelMode_PerProcessor(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	future := now.Add(10 * 24 * time.Hour)

	t.Run("stripe is reversible", func(t *testing.T) {
		sub := baseSub(models.ProcessorStripe, models.StatusActive)
		require.Equal(t, CancelModeReversible, CancelModeFor(sub, now))
	})

	t.Run("ccbill is external_portal", func(t *testing.T) {
		sub := baseSub(models.ProcessorCCBill, models.StatusActive)
		require.Equal(t, CancelModeExternalPortal, CancelModeFor(sub, now))
	})

	t.Run("solana is destructive", func(t *testing.T) {
		sub := baseSub(models.ProcessorSolana, models.StatusActive)
		require.Equal(t, CancelModeDestructive, CancelModeFor(sub, now))
	})

	t.Run("nmi/mobius destructive when no deferred delete", func(t *testing.T) {
		sub := baseSub(models.ProcessorMobius, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		require.Equal(t, CancelModeDestructive, CancelModeFor(sub, now))
	})

	t.Run("nmi/mobius reversible only while deferred delete pending in-window", func(t *testing.T) {
		sub := baseSub(models.ProcessorMobius, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		sub.DeletionScheduledAt = ptrTime(future.Add(-48 * time.Hour))
		require.Equal(t, CancelModeReversible, CancelModeFor(sub, now))
	})

	t.Run("nmi/mobius destructive once delete executed (DeletionScheduledAt cleared)", func(t *testing.T) {
		sub := baseSub(models.ProcessorMobius, models.StatusCancelled)
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
		sub := baseSub(models.ProcessorStripe, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		require.True(t, Resumable(sub, now))
		require.True(t, CancelScheduled(sub, now))
	})

	t.Run("stripe active is NOT resumable", func(t *testing.T) {
		sub := baseSub(models.ProcessorStripe, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		require.False(t, Resumable(sub, now))
		require.False(t, CancelScheduled(sub, now))
	})

	t.Run("expired stripe cancelled is NOT resumable", func(t *testing.T) {
		sub := baseSub(models.ProcessorStripe, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(past)
		require.False(t, Resumable(sub, now))
		require.False(t, CancelScheduled(sub, now))
	})

	t.Run("ccbill cancelled is NOT resumable", func(t *testing.T) {
		sub := baseSub(models.ProcessorCCBill, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		require.False(t, Resumable(sub, now))
		require.Equal(t, CancelModeExternalPortal, CancelModeFor(sub, now))
	})

	t.Run("nmi cancelled without deferred delete is NOT resumable", func(t *testing.T) {
		sub := baseSub(models.ProcessorMobius, models.StatusCancelled)
		sub.CurrentPeriodEndsAt = ptrTime(future)
		sub.DeletionScheduledAt = nil
		require.False(t, Resumable(sub, now))
	})

	t.Run("nmi cancelled with deferred delete in-window IS resumable", func(t *testing.T) {
		sub := baseSub(models.ProcessorMobius, models.StatusCancelled)
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

	ccbill := baseSub(models.ProcessorCCBill, models.StatusCancelled)
	url := CancelPortalURL(ccbill, now)
	require.NotNil(t, url)
	require.Equal(t, CCBillSupportPortalURL, *url)

	stripe := baseSub(models.ProcessorStripe, models.StatusCancelled)
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
			s := baseSub(models.ProcessorStripe, models.StatusCancelled)
			s.CurrentPeriodEndsAt = ptrTime(future)
			return s
		}(),
		func() *models.Subscription {
			s := baseSub(models.ProcessorStripe, models.StatusActive)
			s.CurrentPeriodEndsAt = ptrTime(future)
			return s
		}(),
		func() *models.Subscription {
			s := baseSub(models.ProcessorStripe, models.StatusCancelled)
			s.CurrentPeriodEndsAt = ptrTime(past)
			return s
		}(),
		func() *models.Subscription {
			s := baseSub(models.ProcessorCCBill, models.StatusCancelled)
			s.CurrentPeriodEndsAt = ptrTime(future)
			return s
		}(),
		func() *models.Subscription {
			s := baseSub(models.ProcessorMobius, models.StatusCancelled)
			s.CurrentPeriodEndsAt = ptrTime(future)
			s.DeletionScheduledAt = ptrTime(future.Add(-48 * time.Hour))
			return s
		}(),
		func() *models.Subscription {
			s := baseSub(models.ProcessorMobius, models.StatusCancelled)
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
		sub := baseSub(models.ProcessorMobius, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(periodEnd)

		deleteAt, defer_ := NMIDeferredDeleteAt(sub, now)
		require.True(t, defer_)
		require.Equal(t, periodEnd.Add(-NMIDeleteSafetyMargin), deleteAt)
		require.True(t, deleteAt.After(now))
	})

	t.Run("cancel exactly at the 48h boundary deletes immediately", func(t *testing.T) {
		// period_end - 48h == now  => deleteAt is not strictly after now => immediate.
		periodEnd := now.Add(NMIDeleteSafetyMargin)
		sub := baseSub(models.ProcessorMobius, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(periodEnd)

		_, defer_ := NMIDeferredDeleteAt(sub, now)
		require.False(t, defer_)
	})

	t.Run("cancel within 48h of rebill deletes immediately", func(t *testing.T) {
		periodEnd := now.Add(12 * time.Hour) // well inside the 48h window
		sub := baseSub(models.ProcessorMobius, models.StatusActive)
		sub.CurrentPeriodEndsAt = ptrTime(periodEnd)

		_, defer_ := NMIDeferredDeleteAt(sub, now)
		require.False(t, defer_)
	})

	t.Run("unknown period end deletes immediately", func(t *testing.T) {
		sub := baseSub(models.ProcessorMobius, models.StatusActive)
		sub.CurrentPeriodEndsAt = nil
		_, defer_ := NMIDeferredDeleteAt(sub, now)
		require.False(t, defer_)
	})

	t.Run("past period end deletes immediately", func(t *testing.T) {
		sub := baseSub(models.ProcessorMobius, models.StatusActive)
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
	sub := baseSub(models.ProcessorMobius, models.StatusCancelled)
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
