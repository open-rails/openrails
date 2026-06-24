package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestAssertActiveTransitionAllowed_BlocksChargebackCancelType(t *testing.T) {
	t.Parallel()

	svc := &SubscriptionLifecycleService{}
	cancelType := models.CancelTypeChargeback
	sub := &models.Subscription{ID: uuid.New(), Rail: models.RailCCBill, Status: models.StatusCancelled, CancelType: &cancelType}

	err := svc.assertActiveTransitionAllowed(context.Background(), sub, "renewal", false)
	require.Error(t, err)
	require.True(t, IsTerminalTransitionBlocked(err))
}

func TestAssertActiveTransitionAllowed_BlocksUserAndMerchantCancelTypes(t *testing.T) {
	t.Parallel()

	svc := &SubscriptionLifecycleService{}
	for _, cancelType := range []models.CancelType{models.CancelTypeUser, models.CancelTypeMerchant} {
		cancelType := cancelType
		t.Run(string(cancelType), func(t *testing.T) {
			t.Parallel()
			sub := &models.Subscription{ID: uuid.New(), Rail: models.RailStripe, Status: models.StatusCancelled, CancelType: &cancelType}

			err := svc.assertActiveTransitionAllowed(context.Background(), sub, "renewal", false)
			require.Error(t, err)
			require.True(t, IsTerminalTransitionBlocked(err))
		})
	}
}

func TestReactivateMembership_RequiresFuturePaidThroughDate(t *testing.T) {
	t.Parallel()

	svc := &SubscriptionLifecycleService{}
	past := time.Now().UTC().Add(-time.Hour)

	_, err := svc.ReactivateMembership(context.Background(), &ReactivateMembershipParams{
		Rail:                models.RailCCBill,
		RailSubscriptionID:  "sub_123",
		CurrentPeriodEndsAt: &past,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "reactivation requires a future paid-through period end")
}

func TestAssertActiveTransitionAllowed_IgnoresChargebackFeedbackWithoutCancelType(t *testing.T) {
	t.Parallel()

	svc := &SubscriptionLifecycleService{}
	feedback := "CHARGEBACK: unauthorized"
	sub := &models.Subscription{ID: uuid.New(), Rail: models.RailCCBill, Status: models.StatusCancelled, CancelFeedback: &feedback}

	err := svc.assertActiveTransitionAllowed(context.Background(), sub, "reactivation", false)
	require.NoError(t, err)
}

func TestAssertActiveTransitionAllowed_AllowsExpiredCancelType(t *testing.T) {
	t.Parallel()

	svc := &SubscriptionLifecycleService{}
	cancelType := models.CancelTypeExpired
	sub := &models.Subscription{ID: uuid.New(), Rail: models.RailCCBill, Status: models.StatusCancelled, CancelType: &cancelType}

	err := svc.assertActiveTransitionAllowed(context.Background(), sub, "reactivation", false)
	require.NoError(t, err)
}

func TestAssertActiveTransitionAllowed_AllowsOverride(t *testing.T) {
	t.Parallel()

	svc := &SubscriptionLifecycleService{}
	cancelType := models.CancelTypeChargeback
	sub := &models.Subscription{ID: uuid.New(), Rail: models.RailCCBill, Status: models.StatusCancelled, CancelType: &cancelType}

	err := svc.assertActiveTransitionAllowed(context.Background(), sub, "reactivation", true)
	require.NoError(t, err)
}
