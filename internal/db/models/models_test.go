package models

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
)

func ptr[T any](v T) *T { return &v }

func TestGenMappingPreservesStoredValues(t *testing.T) {
	require.Nil(t, IntPtrTo32(nil))
	require.Nil(t, DerefIntPtr(nil))
	for in, want := range map[int]int32{5: 5, math.MaxInt32 + 1: math.MaxInt32, math.MinInt32 - 1: math.MinInt32} {
		require.Equal(t, want, *IntPtrTo32(ptr(in)), "clamp, never wrap")
	}

	payment, err := PaymentFromGen(gen.OpenrailsPayment{Rail: "paypal", Metadata: []byte(`{"order_id":"o-1"}`)})
	require.NoError(t, err)
	require.Equal(t, Rail("paypal"), payment.Rail, "an unregistered persisted rail is preserved, not rejected")
	require.Equal(t, "o-1", payment.Metadata["order_id"])
	_, err = PaymentFromGen(gen.OpenrailsPayment{Metadata: []byte(`{`)})
	require.ErrorContains(t, err, "payments.metadata")

	product, err := ProductFromGen(gen.OpenrailsProduct{TierRank: 3, EntitlementsSpec: []byte(`{"forever":null,"day":24}`)})
	require.NoError(t, err)
	require.Equal(t, 3, product.TierRank)
	require.Nil(t, product.EntitlementsSpec["forever"])
	require.Contains(t, product.EntitlementsSpec, "forever", "an indefinite entitlement is a nil value, not a missing key")
	require.Equal(t, 24, *product.EntitlementsSpec["day"])

	sub, err := SubscriptionFromGen(gen.OpenrailsSubscription{RetryAttempts: ptr(int32(2)), CancelType: ptr("user"), CollectionPolicy: "engine"})
	require.NoError(t, err)
	require.Equal(t, 2, *sub.RetryAttempts)
	require.Equal(t, CancelType("user"), *sub.CancelType)
	require.Equal(t, uuid.Nil, sub.PriceID)
	require.True(t, sub.CollectionPolicy.Valid())

	raw, err := ToJSONB[map[string]int](nil)
	require.NoError(t, err)
	require.Nil(t, raw, "a nil map is SQL NULL")

	clone := CloneEntitlementsSpec(product.EntitlementsSpec)
	*clone["day"] = 1
	require.Equal(t, 24, *product.EntitlementsSpec["day"], "clone must not alias")
	require.Nil(t, CloneEntitlementsSpec(map[string]*int{}))
}

func TestProductAccessGrantIsActiveAt(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	before, after := now.Add(-time.Hour), now.Add(time.Hour)
	active := ProductAccessStatusActive
	for name, tc := range map[string]struct {
		grant ProductAccessGrant
		want  bool
	}{
		"indefinite and started": {ProductAccessGrant{Status: active, StartsAt: before}, true},
		"starts exactly now":     {ProductAccessGrant{Status: active, StartsAt: now}, true},
		"within window":          {ProductAccessGrant{Status: active, StartsAt: before, EndsAt: &after}, true},
		"not yet started":        {ProductAccessGrant{Status: active, StartsAt: after}, false},
		"ends exactly now":       {ProductAccessGrant{Status: active, StartsAt: before, EndsAt: &now}, false},
		"revoked status":         {ProductAccessGrant{Status: ProductAccessStatusRevoked, StartsAt: before}, false},
		"revoked_at set":         {ProductAccessGrant{Status: active, StartsAt: before, RevokedAt: &now}, false},
	} {
		require.Equal(t, tc.want, tc.grant.IsActiveAt(now), name)
	}
	require.False(t, (*ProductAccessGrant)(nil).IsActiveAt(now))
}

func TestPriceLinksAreAccountKeyed(t *testing.T) {
	psp := uuid.New()
	p := &Price{PSPLinks: map[string]map[string]string{
		"mobius": {RailKeyRail: "nmi", RailKeyPlanID: "premium_new", RailKeyPSPID: psp.String()},
		"stripe": {RailKeyRail: "stripe", RailKeyStripePriceID: " price_123 "},
		"ccbill": {RailKeyRail: "ccbill", RailKeyCCBillFormName: "form-name"},
	}}
	require.Equal(t, "premium_new", p.PSPLinkForRail(RailNMI)[RailKeyPlanID])
	id, ok := p.GetStripeConfig()
	require.True(t, ok)
	require.Equal(t, "price_123", id)
	require.True(t, p.HasRail(RailNMI))
	require.False(t, p.HasRail(RailSolana))
	_, _, ok = p.GetCCBillFlexForm()
	require.False(t, ok, "a FlexForm needs both form_name and flex_id")

	only := p.ForPSP(psp)
	require.Len(t, only.PSPLinks, 1)
	require.Len(t, p.PSPLinks, 3, "ForPSP must not mutate the catalog row")

	// Two accounts on one rail: enumerate both, never guess one.
	p.PSPLinks["paykings"] = map[string]string{RailKeyRail: "nmi", RailKeyPlanID: "premium_pk"}
	require.Len(t, p.PSPLinksForRail(RailNMI), 2)
	require.Nil(t, p.PSPLinkForRail(RailNMI))

	var fresh Price
	fresh.SetCCBillConfig("form", "flex")
	form, flex, ok := fresh.GetCCBillFlexForm()
	require.True(t, ok)
	require.Equal(t, []string{"form", "flex"}, []string{form, flex})
	require.Equal(t, "ccbill", fresh.PSPLinks["ccbill"][RailKeyRail])
}

func TestPriceCadenceAndPurchasability(t *testing.T) {
	for _, archived := range []bool{false, true} {
		require.Equal(t, !archived, (&Product{Archived: archived}).IsPurchasable())
		require.Equal(t, !archived, (&Price{Archived: archived}).IsPurchasable())
	}
	require.Nil(t, (&Price{AccessDurationHours: ptr(720)}).RecurringCycleHours(), "a one-off window is not a cycle")
	require.Nil(t, (&Price{AutoRenew: true}).RecurringCycleDays())
	require.Equal(t, 1, *(&Price{AutoRenew: true, AccessDurationHours: ptr(36)}).RecurringCycleDays(), "provider day cadence floors")
	require.Equal(t, 0, *(&Price{AutoRenew: true, AccessDurationHours: ptr(12)}).RecurringCycleDays())

	_, _, ok := (&Price{TrialUnitAmount: ptr(int64(0))}).GetTrial()
	require.False(t, ok, "a trial needs both amount and duration")
	amount, hours, ok := (&Price{TrialUnitAmount: ptr(int64(0)), TrialDurationHours: ptr(168)}).GetTrial()
	require.True(t, ok)
	require.Equal(t, []int64{0, 168}, []int64{amount, int64(hours)}, "zero is a free trial")

	end := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	sub := &Subscription{CurrentPeriodEndsAt: &end, Status: StatusPastDue, CancelledAt: &end}
	require.Error(t, sub.ActivateWithPrice(&Price{}))
	require.NoError(t, sub.ActivateWithPrice(&Price{ID: uuid.New(), AutoRenew: true, AccessDurationHours: ptr(720)}))
	require.Equal(t, end, *sub.CurrentPeriodStartsAt, "renewal continues from the prior period end")
	require.Equal(t, end.Add(720*time.Hour), *sub.CurrentPeriodEndsAt)
	require.Equal(t, StatusActive, sub.Status)
	require.Nil(t, sub.CancelledAt)
}

// Mirrors the DB CHECKs payments_psp_required_on_rail and
// invoice_payments_psp_required_on_rail: only channels may omit a PSP.
func TestOffRailChannelsAndMoneyMovement(t *testing.T) {
	for _, rail := range []string{"manual", "admin", "MANUAL", " Admin "} {
		require.True(t, IsOffRailChannel(rail), rail)
	}
	for _, rail := range []string{"nmi", "stripe", "ccbill", "solana", "mobius", ""} {
		require.False(t, IsOffRailChannel(rail), rail)
	}
	require.False(t, MoneyMovementUndeclared.Valid(), "undeclared must not pass for none")
	require.True(t, MoneyMovementNone.Valid())
	require.True(t, MoneyMovementRail.Valid())
}
