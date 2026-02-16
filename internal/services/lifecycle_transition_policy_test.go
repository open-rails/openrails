package services

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestAssertActiveTransitionAllowed_BlocksChargebackCancelType(t *testing.T) {
	t.Parallel()

	svc := &SubscriptionLifecycleService{}
	cancelType := models.CancelTypeChargeback
	sub := &models.Subscription{
		ID:         uuid.New(),
		Processor:  models.ProcessorCCBill,
		Status:     models.StatusCancelled,
		CancelType: &cancelType,
	}

	err := svc.assertActiveTransitionAllowed(context.Background(), sub, "renewal", false)
	require.Error(t, err)
	require.True(t, IsTerminalTransitionBlocked(err))
}

func TestAssertActiveTransitionAllowed_BlocksLegacyChargebackFeedback(t *testing.T) {
	t.Parallel()

	svc := &SubscriptionLifecycleService{}
	feedback := "CHARGEBACK: unauthorized"
	sub := &models.Subscription{
		ID:             uuid.New(),
		Processor:      models.ProcessorCCBill,
		Status:         models.StatusCancelled,
		CancelFeedback: &feedback,
	}

	err := svc.assertActiveTransitionAllowed(context.Background(), sub, "reactivation", false)
	require.Error(t, err)
	require.True(t, IsTerminalTransitionBlocked(err))
}

func TestAssertActiveTransitionAllowed_AllowsExpiredCancelType(t *testing.T) {
	t.Parallel()

	svc := &SubscriptionLifecycleService{}
	cancelType := models.CancelTypeExpired
	sub := &models.Subscription{
		ID:         uuid.New(),
		Processor:  models.ProcessorCCBill,
		Status:     models.StatusCancelled,
		CancelType: &cancelType,
	}

	err := svc.assertActiveTransitionAllowed(context.Background(), sub, "reactivation", false)
	require.NoError(t, err)
}

func TestAssertActiveTransitionAllowed_AllowsOverride(t *testing.T) {
	t.Parallel()

	svc := &SubscriptionLifecycleService{}
	cancelType := models.CancelTypeChargeback
	sub := &models.Subscription{
		ID:         uuid.New(),
		Processor:  models.ProcessorCCBill,
		Status:     models.StatusCancelled,
		CancelType: &cancelType,
	}

	err := svc.assertActiveTransitionAllowed(context.Background(), sub, "reactivation", true)
	require.NoError(t, err)
}
