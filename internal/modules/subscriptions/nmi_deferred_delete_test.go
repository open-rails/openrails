package subscriptions

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

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
