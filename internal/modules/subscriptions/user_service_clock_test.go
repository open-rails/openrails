package subscriptions

import (
	"context"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestUserSubscriptionResponseUsesReadTimeForEligibility(t *testing.T) {
	// Long past in physical time, still inside the simulated paid period.
	now := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	end := now.Add(time.Hour)
	clock := clockwork.NewFakeClockAt(now)
	svc := &UserSubscriptionService{}
	svc.SetClock(clock)
	response := &UserSubscriptionResponse{Subscription: &models.Subscription{Rail: models.RailStripe, Status: models.StatusCancelled, CurrentPeriodEndsAt: &end}}
	svc.enrichSubscriptionResponse(context.Background(), response)
	check := func(want bool) {
		t.Helper()
		view := response.View()
		require.Equal(t, want, view.Resumable)
		require.Equal(t, want, view.CancelScheduled)
	}
	check(true)
	clock.Advance(2 * time.Hour)
	// The response retains its consistent read snapshot until read again.
	check(true)
	svc.enrichSubscriptionResponse(context.Background(), response)
	check(false)
}
