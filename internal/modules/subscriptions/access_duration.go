package subscriptions

import (
	"errors"
	"math"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
)

func validateAccessDuration(hours *int) error {
	if hours != nil && (*hours <= 0 || int64(*hours) > math.MaxInt64/int64(time.Hour)) {
		return errors.New("access duration must be positive whole hours within the supported duration range")
	}
	return nil
}

func accessEnd(start time.Time, hours *int) *time.Time {
	if hours == nil {
		return nil
	}
	end := start.Add(time.Duration(*hours) * time.Hour)
	return &end
}

// subscriptionAccessEnd preserves the accepted distinction: provider DEFAULT
// ends at its verified service boundary; an explicit duration remains hourly.
func subscriptionAccessEnd(sub *models.Subscription, start time.Time) *time.Time {
	if sub.ProviderAccessDefaultSnapshot {
		return sub.CurrentPeriodEndsAt
	}
	return accessEnd(start, sub.AccessDurationHoursSnapshot)
}

// subscriptionAccess grants the subscription's product for exactly the
// accepted access window. Billing periods schedule charges and never supply an
// implicit expiry for a new grant.
func subscriptionAccess(sub *models.Subscription, start time.Time) entitlements.PushAccessParams {
	end := subscriptionAccessEnd(sub, start)
	return entitlements.PushAccessParams{
		UserID: sub.CustomerID.String(), ProductID: sub.ProductID, NotBefore: &start,
		EndsAt: end, Indefinite: end == nil && !sub.ProviderAccessDefaultSnapshot,
		SourceType: models.AccessSourceSubscription, SourceID: sub.ID.String(), Quantity: sub.Quantity,
	}
}

// Renewal grace applies to continuous membership access. A deliberately shorter
// access window is not extended through an unpaid gap, and a longer or indefinite
// purchase does not need a billing-boundary grace grant.
func accessMatchesBillingPeriod(sub *models.Subscription, start, end time.Time) bool {
	if sub.ProviderAccessDefaultSnapshot {
		return sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.Equal(end)
	}
	if sub.AccessDurationHoursSnapshot == nil {
		return false
	}
	if sub.CollectionPolicy != models.CollectionPolicyEngine && sub.Price != nil && sub.Price.BillingIntervalHours != nil {
		return *sub.AccessDurationHoursSnapshot == *sub.Price.BillingIntervalHours
	}
	return end.Sub(start) == time.Duration(*sub.AccessDurationHoursSnapshot)*time.Hour
}
