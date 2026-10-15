package subscriptions

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionFailureNoticeUsesCommittedCollectionDecision(t *testing.T) {
	next := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	code, reason := "223", "expired_card"
	for _, tc := range []struct {
		name   string
		status models.SubscriptionStatus
		held   bool
		next   *time.Time
		retry  billing.NotificationRetryStatus
		action billing.NotificationRecoveryAction
	}{
		{"scheduled", models.StatusPastDue, false, &next, billing.NotificationRetryScheduled, billing.NotificationRecoveryUpdatePaymentMethod},
		{"method required", models.StatusAwaitingMethod, false, &next, billing.NotificationRetryPaymentMethodRequired, billing.NotificationRecoveryUpdatePaymentMethod},
		{"stopped", models.StatusCanceled, false, &next, billing.NotificationRetryStopped, billing.NotificationRecoveryResubscribe},
		{"held", models.StatusPastDue, true, &next, billing.NotificationRetryUnknown, billing.NotificationRecoveryContactSupport},
		{"provider schedule unknown", models.StatusPastDue, false, nil, billing.NotificationRetryUnknown, billing.NotificationRecoveryContactSupport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := &models.Subscription{ID: uuid.New(), ProductID: uuid.New(), Status: tc.status, NextRetryAt: tc.next, Rail: models.RailNMI}
			data := subscriptionFailureNotice(sub, &FailMembershipParams{FailureCode: &code, FailureReason: &reason, Decline: decline.FixPaymentMethod}, tc.held)
			require.Equal(t, tc.retry, data.RetryStatus)
			require.Equal(t, tc.action, data.RecoveryAction)
			require.Equal(t, billing.SubscriptionID(sub.ID), data.SubscriptionID)
			require.Equal(t, code, data.FailureCode)
			require.Equal(t, reason, data.FailureReason)
			if tc.retry == billing.NotificationRetryScheduled {
				require.Equal(t, &next, data.NextAttemptAt)
			} else {
				require.Nil(t, data.NextAttemptAt, "an unscheduled outcome cannot promise another attempt")
			}
		})
	}
}

func TestSubscriptionFailureNoticeKeepsKnownReasonWhenProviderTextIsMissing(t *testing.T) {
	code := "223"
	data := subscriptionFailureNotice(&models.Subscription{Rail: models.RailNMI, Status: models.StatusAwaitingMethod}, &FailMembershipParams{FailureCode: &code}, false)
	require.Equal(t, string(billing.DeclineExpiredCard), data.FailureReason)
}
