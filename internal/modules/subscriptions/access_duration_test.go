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
		require.NoError(t, pushRenewalGrace(context.Background(), nil, r, sub, []string{"premium"}, start, billingEnd))
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

func TestExplicitRevokeAfterCanceledBillingPeriod(t *testing.T) {
	end := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	snapshot := lifecycle.Snapshot{Status: lifecycle.Canceled, Owner: lifecycle.Engine, PaidThrough: end, EndedAt: end, CancelKind: lifecycle.CancelUser}
	_, effects, err := lifecycle.Apply(snapshot, lifecycle.Cancel{Kind: lifecycle.CancelMerchant, Immediate: true, At: end.Add(time.Hour)})
	require.NoError(t, err)
	require.Contains(t, effects, lifecycle.EndAccess{At: end.Add(time.Hour), Revoke: true}, "refund still revokes longer or perpetual access after recurrence has ended")
}

func TestAcceptedLegacyUpgradeRetainsPartialPeriodAccess(t *testing.T) {
	var legacy NMIUpgradePayload
	require.NoError(t, json.Unmarshal([]byte(`{"period_start":"2026-10-08T12:45:00Z","period_end":"2026-10-08T13:00:00Z"}`), &legacy))
	require.Nil(t, legacy.AccessDurationHours, "an accepted fifteen-minute remainder is never rounded to zero hours")
	require.Equal(t, legacy.PeriodEnd, *legacy.AccessEndsAt)
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	var restored NMIUpgradePayload
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.Equal(t, legacy, restored, "retries retain the original accepted access end")
	var indefinite NMIUpgradePayload
	require.NoError(t, json.Unmarshal([]byte(`{"period_start":"2026-10-08T12:45:00Z","period_end":"2026-10-08T13:00:00Z","access_duration_hours":null}`), &indefinite))
	require.Nil(t, indefinite.AccessEndsAt, "explicit null is independent of the billing boundary")
}

func TestLatePaidPeriodRetainsExpiredAccessHistory(t *testing.T) {
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	now := start.Add(48 * time.Hour)
	recorded := &recordedAccess{}
	svc := &SubscriptionLifecycleService{clock: clockwork.NewFakeClockAt(now), entitlementServiceFactory: func(*db.DB, clockwork.Clock) lifecycleEntitlementService { return recorded }}
	sub := &models.Subscription{ID: uuid.New(), CustomerID: uuid.New(), AccessDurationHoursSnapshot: new(24), EntitlementsSpecSnapshot: map[string]*int{"premium": nil}}
	_, err := svc.ApplyEffects(t.Context(), nil, sub, []lifecycle.Effect{lifecycle.GrantPeriod{Start: start, End: start.Add(720 * time.Hour)}}, now, EffectOptions{})
	require.NoError(t, err)
	require.Len(t, recorded.grants, 1, "late settlement still records what was bought")
	require.Equal(t, start.Add(24*time.Hour), *recorded.grants[0].EndsAt, "it never invents fresh access from settlement time")
}

func TestNativeGraceUsesDeclaredCadenceAndKeepsPaidWindow(t *testing.T) {
	start := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	providerEnd := start.Add(6 * time.Hour) // date-only provider boundary
	paidEnd := start.Add(24 * time.Hour)
	sub := &models.Subscription{ID: uuid.New(), CustomerID: uuid.New(), Status: models.StatusPastDue, CollectionPolicy: models.CollectionPolicyNMISchedule,
		Price: &models.Price{BillingIntervalHours: new(24)}, AccessDurationHoursSnapshot: new(24), CurrentPeriodStartsAt: &start, CurrentPeriodEndsAt: &providerEnd,
		EntitlementsSpecSnapshot: map[string]*int{"premium": nil}, DunningPolicy: []byte(`{"access_during_dunning":"keep","access_while_renewal_held":"keep"}`)}
	r := &recordedAccess{}
	require.NoError(t, pushRenewalGrace(t.Context(), nil, r, sub, []string{"premium"}, start, providerEnd))
	require.Len(t, r.grants, 1)
	require.Equal(t, models.EntitlementSourceGrace, r.grants[0].SourceType)
	require.Equal(t, paidEnd, *r.grants[0].NotBefore, "provider date rounding never shortens purchased access")
	require.True(t, r.grants[0].Indefinite, "existing keep policy is represented by explicit grace")
	longerProvider := &recordedAccess{}
	require.NoError(t, pushRenewalGrace(t.Context(), nil, longerProvider, sub, []string{"premium"}, start, start.Add(48*time.Hour)))
	require.Equal(t, paidEnd, *longerProvider.grants[0].NotBefore, "a longer provider billing period leaves no gap after matched paid access ends")
	suspended := &recordedAccess{}
	svc := &SubscriptionLifecycleService{clock: clockwork.NewFakeClockAt(paidEnd), entitlementServiceFactory: func(*db.DB, clockwork.Clock) lifecycleEntitlementService { return suspended }}
	sub.DunningPolicy = []byte(`{"access_during_dunning":"suspend","access_while_renewal_held":"keep"}`)
	require.NoError(t, svc.EnsureRenewalGrace(t.Context(), nil, sub))
	require.Empty(t, suspended.grants, "importing past-due billing honors its dunning policy, independently of held-renewal policy")
	require.Equal(t, []models.EntitlementSourceType{models.EntitlementSourceGrace}, suspended.revoked)
	sub.AccessDurationHoursSnapshot = new(1)
	require.NoError(t, pushRenewalGrace(t.Context(), nil, r, sub, []string{"premium"}, start, providerEnd))
	require.Len(t, r.grants, 1, "deliberately short access never receives recurring grace")
	sub.AccessDurationHoursSnapshot = new(24)
	sub.Status = models.StatusCanceled
	require.NoError(t, pushRenewalGrace(t.Context(), nil, r, sub, []string{"premium"}, start, providerEnd))
	require.Len(t, r.grants, 1, "canceled recurrence never regains grace")
}
