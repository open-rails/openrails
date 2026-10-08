package subscriptions

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionAccessComesFromLiveEntitlementWindows(t *testing.T) {
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	customer, otherCustomer := uuid.New(), uuid.New()
	newResponse := func(status models.SubscriptionStatus, hours *int) *UserSubscriptionResponse {
		return &UserSubscriptionResponse{Subscription: &models.Subscription{ID: uuid.New(), CustomerID: customer, Rail: models.RailNMI, Status: status, StartedAt: start, CurrentPeriodEndsAt: new(start.Add(720 * time.Hour)), AccessDurationHoursSnapshot: hours}}
	}
	short := newResponse(models.StatusActive, new(24))
	long := newResponse(models.StatusCanceled, new(1000))
	perpetual := newResponse(models.StatusCanceled, nil)
	revoked := newResponse(models.StatusCanceled, nil)
	grace := newResponse(models.StatusPastDue, new(720))
	// A stale response must not retain access when its current windows are gone.
	short.Access, revoked.Access = &billing.SubscriptionAccess{}, &billing.SubscriptionAccess{}
	window := func(sub *UserSubscriptionResponse, end *time.Time, source models.EntitlementSourceType) models.Entitlement {
		return models.Entitlement{CustomerID: customer, Entitlement: "premium", SourceID: &sub.ID, SourceType: source, StartsAt: start, EndsAt: end}
	}
	active := map[uuid.UUID][]models.Entitlement{customer: {
		window(long, new(start.Add(800*time.Hour)), models.EntitlementSourceSubscription),
		window(long, new(start.Add(1000*time.Hour)), models.EntitlementSourceSubscription),
		window(perpetual, nil, models.EntitlementSourceSubscription),
		window(grace, new(start.Add(800*time.Hour)), models.EntitlementSourceGrace),
	}, otherCustomer: {window(revoked, nil, models.EntitlementSourceSubscription)}}
	applySubscriptionAccess([]*UserSubscriptionResponse{short, long, perpetual, revoked, grace}, active)
	require.Nil(t, short.Access, "expired short access cannot be inferred from active billing")
	require.Nil(t, revoked.Access, "revoked access cannot be inferred from a perpetual snapshot or another customer")
	require.Equal(t, start.Add(1000*time.Hour), *long.Access.EndsAt, "normal cancellation retains the longest paid live window")
	require.NotNil(t, perpetual.Access)
	require.Nil(t, perpetual.Access.EndsAt)
	require.Equal(t, billing.SubscriptionID(grace.ID), grace.Access.SubscriptionID, "explicit renewal grace remains visible")
	applySubscriptionAccess([]*UserSubscriptionResponse{perpetual}, nil)
	require.Nil(t, perpetual.Access, "explicit revoke removes the formerly perpetual access response")
}
