package service

import (
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestSubscriptionDTOKeepsBusinessEvaluationTime(t *testing.T) {
	now := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	end := now.Add(time.Hour)
	response := &subscriptions.UserSubscriptionResponse{EvaluatedAt: now, Subscription: &models.Subscription{Rail: models.RailStripe, Status: models.StatusCancelled, CurrentPeriodEndsAt: &end}}
	dto := subscriptionFromModel(response)
	require.True(t, dto.Resumable)
	require.True(t, dto.CancelScheduled)
	response.EvaluatedAt = end
	dto = subscriptionFromModel(response)
	require.False(t, dto.Resumable)
	require.False(t, dto.CancelScheduled)
}
