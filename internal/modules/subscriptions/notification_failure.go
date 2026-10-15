package subscriptions

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// subscriptionFailureNotice describes the committed collection decision. It
// never derives a retry promise from a decline code alone.
func subscriptionFailureNotice(sub *models.Subscription, failure *FailMembershipParams, held bool) billing.NotificationData {
	data := billing.NotificationData{
		SubscriptionID: billing.SubscriptionID(sub.ID), ProductID: billing.ProductID(sub.ProductID),
		FailureCode: normalize.FromPtr(failure.FailureCode), FailureReason: normalize.FromPtr(failure.FailureReason),
		DeclineOutcome: failure.Decline.String(), Rail: string(sub.Rail), RailSubscriptionID: sub.RailSubscriptionID,
		RetryStatus: billing.NotificationRetryUnknown, RecoveryAction: billing.NotificationRecoveryContactSupport,
	}
	if data.FailureReason == "" && data.FailureCode != "" {
		data.FailureReason = decline.ReasonFor(string(sub.Rail), data.FailureCode)
	}
	switch {
	case held:
		// The engine cannot confirm whether collection should stop or resume.
	case sub.Status == models.StatusCanceled:
		data.RetryStatus, data.RecoveryAction = billing.NotificationRetryStopped, billing.NotificationRecoveryResubscribe
	case sub.Status == models.StatusAwaitingMethod:
		data.RetryStatus, data.RecoveryAction = billing.NotificationRetryPaymentMethodRequired, billing.NotificationRecoveryUpdatePaymentMethod
	case sub.NextRetryAt != nil:
		next := sub.NextRetryAt.UTC()
		data.NextAttemptAt = &next
		data.RetryStatus, data.RecoveryAction = billing.NotificationRetryScheduled, billing.NotificationRecoveryUpdatePaymentMethod
	}
	return data
}
