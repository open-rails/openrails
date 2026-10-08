package subscriptions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/stretchr/testify/require"
)

type recordedAccess struct {
	grants  []entitlements.PushNewEntitlementParams
	revoked []models.EntitlementSourceType
}

func (r *recordedAccess) PushNewEntitlement(_ context.Context, p entitlements.PushNewEntitlementParams) (*models.Entitlement, error) {
	r.grants = append(r.grants, p)
	return nil, nil
}
func (*recordedAccess) ListDistinctEntitlementNamesBySource(context.Context, models.EntitlementSourceType, uuid.UUID) ([]string, error) {
	return nil, nil
}
func (*recordedAccess) RevokeExistingEntitlement(context.Context, entitlements.RevokeExistingEntitlementParams) error {
	return nil
}
func (r *recordedAccess) RevokeSourcesForSubscriptionAsOf(_ context.Context, _ string, _ uuid.UUID, _ time.Time, _ models.EntitlementRevokeReason, sources ...models.EntitlementSourceType) error {
	r.revoked = append(r.revoked, sources...)
	return nil
}
func (*recordedAccess) BoundSubscriptionAccess(context.Context, uuid.UUID, time.Time) error {
	panic("paid access must not be bounded by billing")
}
func (*recordedAccess) ResumeSubscriptionAccess(context.Context, uuid.UUID) error { return nil }

func TestBillingPeriodDoesNotDetermineAccess(t *testing.T) {
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	billingEnd := start.Add(720 * time.Hour)
	for _, hours := range []*int{new(24), new(1000), nil} {
		r := &recordedAccess{}
		svc := &SubscriptionLifecycleService{clock: clockwork.NewFakeClockAt(start), entitlementServiceFactory: func(*db.DB, clockwork.Clock) lifecycleEntitlementService { return r }}
		sub := &models.Subscription{ID: uuid.New(), CustomerID: uuid.New(), AccessDurationHoursSnapshot: hours, EntitlementsSpecSnapshot: map[string]*int{"premium": nil}, Status: models.StatusActive, CollectionPolicy: models.CollectionPolicyEngine, CurrentPeriodStartsAt: &start, CurrentPeriodEndsAt: &billingEnd}
		_, err := svc.ApplyEffects(context.Background(), nil, sub, []lifecycle.Effect{lifecycle.GrantPeriod{Start: start, End: billingEnd}}, start, EffectOptions{})
		require.NoError(t, err)
		require.Len(t, r.grants, 1)
		grant := r.grants[0]
		require.Equal(t, start, *grant.NotBefore)
		if hours == nil {
			require.True(t, grant.Indefinite)
			require.Nil(t, grant.EndsAt)
		} else {
			require.False(t, grant.Indefinite)
			require.Equal(t, start.Add(time.Duration(*hours)*time.Hour), *grant.EndsAt)
		}
		require.NoError(t, pushEngineRenewalGrace(context.Background(), nil, r, sub, []string{"premium"}, start, billingEnd))
		require.Len(t, r.grants, 1, "grace must not override deliberate independent access duration")
		require.True(t, EngineCollectionDue(sub, billingEnd, false), "access expiration never stops recurring billing")
		_, err = Transition(sub, lifecycle.Cancel{Kind: lifecycle.CancelUser, At: start.Add(time.Hour)}, start.Add(time.Hour))
		require.NoError(t, err)
		require.False(t, EngineCollectionDue(sub, billingEnd, false), "user cancellation stops the next charge")
		_, err = svc.ApplyEffects(context.Background(), nil, sub, []lifecycle.Effect{lifecycle.EndAccess{At: billingEnd}}, start.Add(time.Hour), EffectOptions{})
		require.NoError(t, err)
		require.Equal(t, []models.EntitlementSourceType{models.EntitlementSourceGrace}, r.revoked, "cancel keeps every paid access grant")
		r.revoked = nil
		_, err = svc.ApplyEffects(context.Background(), nil, sub, []lifecycle.Effect{lifecycle.EndAccess{At: start, Revoke: true}}, start, EffectOptions{})
		require.NoError(t, err)
		require.Contains(t, r.revoked, models.EntitlementSourceSubscription, "explicit revoke still removes paid access")
	}
}

func TestAcceptedAccessDurationReplay(t *testing.T) {
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, field string
		want        *int
	}{
		{"legacy accepted period", "", new(720)},
		{"explicit indefinite", `,"access_duration_hours":null`, nil},
		{"independent finite", `,"access_duration_hours":24`, new(24)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"period_start":"2026-10-08T00:00:00Z","period_end":"2026-11-07T00:00:00Z"` + tc.field + `}`)
			var initial InitialMembershipTerms
			var renewal RenewalTerms
			require.NoError(t, json.Unmarshal(raw, &initial))
			require.NoError(t, json.Unmarshal(raw, &renewal))
			require.Equal(t, tc.want, initial.AccessDurationHours)
			require.Equal(t, tc.want, renewal.AccessDurationHours)
			// Immutable history validates the access interval, independently of billing.
			merchant, customer, subscription := uuid.New(), uuid.New(), uuid.New()
			initial.CustomerID, initial.SubscriptionID = customer, subscription
			initial.Entitlements = map[string]*int{"premium": nil}
			source := subscription.String()
			row := gen.BillingGrant{MerchantID: merchant, CustomerID: customer, Kind: "entitlement", SourceType: "subscription", SourceID: &source, Event: "grant", StartsAt: start, EndsAt: accessEnd(start, tc.want), SpecSnapshot: []byte(`{"entitlements":["premium"]}`)}
			require.NoError(t, ValidateInitialMembershipHistory(merchant, initial, []gen.BillingGrant{row}))
			row.EndsAt = new(start.Add(48 * time.Hour))
			require.Error(t, ValidateInitialMembershipHistory(merchant, initial, []gen.BillingGrant{row}))
		})
	}
}
