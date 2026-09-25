package subscriptions

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/billing/lifecycle"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestTransitionWritesTheDecidedRow(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	next := end.AddDate(0, 1, 0)
	now := start.Add(10 * 24 * time.Hour)
	row := func(status models.SubscriptionStatus) *models.Subscription {
		attempts, retry := 2, end.Add(48*time.Hour)
		s, e := start, end
		return &models.Subscription{Status: status, Rail: models.RailNMI, CollectionPolicy: models.CollectionPolicyNMISchedule,
			CurrentPeriodStartsAt: &s, CurrentPeriodEndsAt: &e, RetryAttempts: &attempts, NextRetryAt: &retry, GraceEndsAt: &retry}
	}

	sub := row(models.StatusPastDue)
	effects, err := Transition(sub, lifecycle.RenewalPaid{PeriodStart: end, PeriodEnd: next}, now)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, sub.Status)
	require.Equal(t, end, *sub.CurrentPeriodStartsAt)
	require.Equal(t, next, *sub.CurrentPeriodEndsAt)
	require.Nil(t, sub.RetryAttempts, "a paid period closes dunning")
	require.Nil(t, sub.NextRetryAt)
	require.Contains(t, effects, lifecycle.Effect(lifecycle.GrantPeriod{Start: end, End: next}))

	sub = row(models.StatusPastDue)
	_, err = Transition(sub, lifecycle.RenewalDeclined{PeriodStart: end, Bucket: lifecycle.FixMethod}, now)
	require.NoError(t, err)
	require.Equal(t, models.StatusAwaitingMethod, sub.Status)
	require.Nil(t, sub.NextRetryAt, "nothing is retried while the card waits")
	require.Equal(t, end, *sub.CurrentPeriodEndsAt)

	sub = row(models.StatusActive)
	sub.RetryAttempts, sub.NextRetryAt, sub.GraceEndsAt = nil, nil, nil
	_, err = Transition(sub, lifecycle.Cancel{Kind: lifecycle.CancelUser, At: now}, now)
	require.NoError(t, err)
	require.Equal(t, models.StatusCancelled, sub.Status)
	require.Equal(t, models.CancelTypeUser, *sub.CancelType)
	require.Equal(t, end, *sub.EndedAt, "a user cancel keeps what was paid")
	require.Equal(t, now, *sub.CancelledAt)

	effects, err = Transition(sub, lifecycle.Resume{At: now.Add(time.Hour)}, now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, sub.Status)
	require.Nil(t, sub.CancelType)
	require.Nil(t, sub.EndedAt)
	require.Equal(t, []lifecycle.Effect{lifecycle.ReopenAccess{}}, effects)

	sub = row(models.StatusActive)
	before := *sub
	effects, err = Transition(sub, lifecycle.RenewalPaid{PeriodStart: start, PeriodEnd: end}, now)
	require.NoError(t, err)
	require.Empty(t, effects, "a replayed payment is a no-op")
	require.Equal(t, before, *sub)

	sub = row(models.StatusActive)
	sub.CollectionPolicy, sub.Rail = models.CollectionPolicyProvider, models.RailStripe
	_, err = Transition(sub, lifecycle.RenewalDeclined{PeriodStart: end, Bucket: lifecycle.NonRecoverable}, now)
	require.NoError(t, err)
	require.Equal(t, models.StatusPastDue, sub.Status, "a provider-owned decline is mirrored, never cancelled here")
}
