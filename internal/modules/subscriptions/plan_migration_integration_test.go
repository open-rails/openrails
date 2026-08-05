//go:build integration

package subscriptions

// #813 plan-migration integration suite — the cross-product generalization of
// the #773 reprice engine, exercised over the SAME real-Postgres fixture
// pattern as reprice_integration_test.go. Covers: boundary + immediate modes
// on engine-driven rails (full entitlement cutover), the Stripe push seam
// (boundary schedule / immediate proration-none update / push-failure
// degradation), capability classification (ccbill/solana/native-gateway →
// blocked), #781 notice-window reuse, preview write-freedom, batch cancel,
// idempotent re-runs, and the converge applied-hook.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

func genApplyParams(subID, toPriceID uuid.UUID) gen.ApplyScheduledRepriceForSubscriptionPriceParams {
	return gen.ApplyScheduledRepriceForSubscriptionPriceParams{SubscriptionID: subID, ToPriceID: toPriceID}
}

// fakeStripePusher records rail pushes instead of calling Stripe.
type fakeStripePusher struct {
	itemID          string
	itemErr         error
	updateErr       error
	scheduleErr     error
	updates         []map[string]string
	schedules       []map[string]string
	itemIDRequests  []string
	scheduleReturns string
}

func (f *fakeStripePusher) GetSubscriptionItemID(_ context.Context, subscriptionID string) (string, error) {
	f.itemIDRequests = append(f.itemIDRequests, subscriptionID)
	if f.itemErr != nil {
		return "", f.itemErr
	}
	if f.itemID == "" {
		return "si_fake", nil
	}
	return f.itemID, nil
}

func (f *fakeStripePusher) UpdateSubscriptionPrice(_ context.Context, subscriptionID, itemID, newPriceID, internalPriceID, prorationBehavior, billingAnchor string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates = append(f.updates, map[string]string{
		"subscription_id": subscriptionID, "item_id": itemID, "new_price_id": newPriceID,
		"internal_price_id": internalPriceID, "proration_behavior": prorationBehavior, "billing_anchor": billingAnchor,
	})
	return nil
}

func (f *fakeStripePusher) ScheduleSubscriptionPriceChange(_ context.Context, subscriptionID, currentPriceID, newPriceID string, _, _ time.Time, _ *int) (string, error) {
	if f.scheduleErr != nil {
		return "", f.scheduleErr
	}
	f.schedules = append(f.schedules, map[string]string{
		"subscription_id": subscriptionID, "current_price_id": currentPriceID, "new_price_id": newPriceID,
	})
	return "sub_sched_fake", nil
}

type pmLookupFunc func(ctx context.Context, id uuid.UUID) (*models.PaymentMethod, error)

func (f pmLookupFunc) GetByID(ctx context.Context, id uuid.UUID) (*models.PaymentMethod, error) {
	return f(ctx, id)
}

// fakeNMIPusher (#815) records gateway-native NMI pushes at the seam level
// (the real Direct Post wire shape is covered by the fake-gateway suite in
// plan_migration_nmi_integration_test.go).
type fakeNMIPusher struct {
	canPush bool
	pushErr error
	pushes  []map[string]any
}

func (f *fakeNMIPusher) CanPush(_ context.Context, sub *models.Subscription) bool {
	return f != nil && f.canPush && strings.TrimSpace(sub.RailSubscriptionID) != ""
}

func (f *fakeNMIPusher) PushPlanAmount(_ context.Context, sub *models.Subscription, currency string, amountNative int64) error {
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushes = append(f.pushes, map[string]any{
		"subscription_id": sub.ID, "rail_subscription_id": sub.RailSubscriptionID,
		"amount_micros": amountNative, "currency": currency,
	})
	return nil
}

type planMigrationFixture struct {
	*repriceFixture
	stripe *fakeStripePusher
	nmi    *fakeNMIPusher
	pm     *PlanMigrationService

	targetProductID uuid.UUID
	targetPriceID   uuid.UUID
	// target price with a stripe psp link
	stripeTargetPriceID uuid.UUID
	stripeSourcePriceID uuid.UUID
}

func newPlanMigrationFixture(t *testing.T) *planMigrationFixture {
	t.Helper()
	base := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	now := base.clock.Now()
	suffix := uuid.NewString()[:8]

	// Target product B with distinct entitlements (cutover must swap specs).
	targetProductID := uuid.New()
	_, err := base.pool.Exec(ctx, `
		INSERT INTO openrails.products (id, merchant_id, key, display_name, entitlements_spec)
		VALUES ($1,$2,$3,$3,'{"plan_b_access": null}'::jsonb)`,
		targetProductID, base.merchantID, "planmig-target-"+suffix)
	require.NoError(t, err)
	// Source product entitlements: plan_a_access (so the diff pass revokes it).
	_, err = base.pool.Exec(ctx, `
		UPDATE openrails.products SET entitlements_spec = '{"plan_a_access": null}'::jsonb WHERE id = $1`, base.productID)
	require.NoError(t, err)

	insertPrice := func(id, productID uuid.UUID, amount int64, key string, psp string) {
		_, e := base.pool.Exec(ctx, `
			INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, access_duration_hours, auto_renew, archived, key, psp_links, created_at, updated_at)
			VALUES ($1,$2,$3,$4,'USD',720,true,false,$5,$6::jsonb,$7,$7)`,
			id, productID, base.merchantID, amount, key, psp, now)
		require.NoError(t, e)
	}
	targetPriceID := uuid.New()
	insertPrice(targetPriceID, targetProductID, 9000000, "planmig-target-"+suffix, `{}`)

	stripeSourcePriceID := uuid.New()
	insertPrice(stripeSourcePriceID, base.productID, 16000000, "planmig-stripe-src-"+suffix,
		`{"stripe": {"rail": "stripe", "price_id": "price_src_`+suffix+`"}}`)
	stripeTargetPriceID := uuid.New()
	insertPrice(stripeTargetPriceID, targetProductID, 14000000, "planmig-stripe-tgt-"+suffix,
		`{"stripe": {"rail": "stripe", "price_id": "price_tgt_`+suffix+`"}}`)

	stripe := &fakeStripePusher{}
	nmiPusher := &fakeNMIPusher{canPush: true}
	// Payment-method lookup: any PM id resolves to an engine-anchored
	// instrument (per-test overrides construct their own service).
	pmLookup := pmLookupFunc(func(_ context.Context, id uuid.UUID) (*models.PaymentMethod, error) {
		return &models.PaymentMethod{ID: id, StoredCredentialRecurringRef: "anchor-" + id.String()}, nil
	})
	svc := NewPlanMigrationService(base.repriceSvc, stripe, nmiPusher, pmLookup)

	f := &planMigrationFixture{
		repriceFixture:      base,
		stripe:              stripe,
		nmi:                 nmiPusher,
		pm:                  svc,
		targetProductID:     targetProductID,
		targetPriceID:       targetPriceID,
		stripeTargetPriceID: stripeTargetPriceID,
		stripeSourcePriceID: stripeSourcePriceID,
	}
	t.Cleanup(func() {
		_, _ = base.pool.Exec(context.Background(), "DELETE FROM openrails.subscriptions WHERE product_id = $1", targetProductID)
		_, _ = base.pool.Exec(context.Background(), "DELETE FROM openrails.prices WHERE product_id = $1", targetProductID)
		_, _ = base.pool.Exec(context.Background(), "DELETE FROM openrails.products WHERE id = $1", targetProductID)
	})
	return f
}

// createSubscriptionOnRail seeds an active subscription with an explicit rail
// + optional payment-method row.
func (f *planMigrationFixture) createSubscriptionOnRail(t *testing.T, ctx context.Context, priceID uuid.UUID, rail string, withPM bool) (subID uuid.UUID, railSubID string) {
	t.Helper()
	var productID uuid.UUID
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT product_id FROM openrails.prices WHERE id = $1`, priceID).Scan(&productID))
	subject := "planmig-customer-" + uuid.NewString()
	var customerID uuid.UUID
	require.NoError(t, f.pool.QueryRow(ctx,
		`INSERT INTO openrails.customers (merchant_id, subject) VALUES ($1,$2) RETURNING id`,
		f.merchantID, subject).Scan(&customerID))
	pspID := dbtest.EnsureTestPSP(ctx, t, f.pool, f.merchantID, rail)
	var pmID *uuid.UUID
	if withPM {
		id := uuid.New()
		_, err := f.pool.Exec(ctx, `
			INSERT INTO openrails.payment_methods (id, customer_id, rail, psp_id, rail_customer_ref, initial_transaction_id, stored_credential_recurring_ref, merchant_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, customerID, rail, pspID, "cust-"+uuid.NewString()[:8], "init-"+uuid.NewString()[:8], "anchor-"+uuid.NewString()[:8], f.merchantID)
		require.NoError(t, err)
		pmID = &id
	}
	railSubID = "planmig-rail-sub-" + uuid.NewString()
	now := f.clock.Now()
	periodEnd := now.Add(30 * 24 * time.Hour)
	require.NoError(t, f.pool.QueryRow(ctx, `
		INSERT INTO openrails.subscriptions (merchant_id, customer_id, product_id, price_id, status, rail, psp_id, rail_subscription_id, payment_method_id, current_period_starts_at, current_period_ends_at, started_at)
		VALUES ($1,$2,$3,$4,'active',$5,$6,$7,$8,$9,$10,$9) RETURNING id`,
		f.merchantID, customerID, productID, priceID, rail, pspID, railSubID, pmID, now, periodEnd).Scan(&subID))
	return subID, railSubID
}

func (f *planMigrationFixture) subscriptionRow(t *testing.T, ctx context.Context, subID uuid.UUID) (priceID, productID uuid.UUID) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT price_id, product_id FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&priceID, &productID))
	return priceID, productID
}

// TestPlanMigration_BoundaryCutoverOnEngineRail: the core mechanic — a
// migration scheduled "now" flips price AND product AND entitlement snapshots
// at the subscription's next renewal, never mid-cycle.
func TestPlanMigration_BoundaryCutoverOnEngineRail(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", true)

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.NotNil(t, res.BatchID)
	require.Equal(t, 1, res.Matched)
	require.Equal(t, 1, res.Scheduled)
	require.Equal(t, 0, res.Blocked)
	require.True(t, res.SourceArchived)

	// Source price archived, target untouched.
	src, err := f.priceSvc.GetByID(ctx, f.lowPriceID)
	require.NoError(t, err)
	require.True(t, src.Archived, "source price must be archived by the migration")

	// Nothing flips mid-cycle.
	priceID, productID := f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.lowPriceID, priceID)
	require.Equal(t, f.productID, productID)

	// The renewal applies the full cross-product cutover and charges the
	// TARGET amount.
	f.clock.Advance(time.Hour)
	amount := f.renewalAmount(t, ctx, models.RailNMI, railSubID)
	require.EqualValues(t, 9000000, amount, "renewal must charge the target plan's amount")
	priceID, productID = f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.targetPriceID, priceID)
	require.Equal(t, f.targetProductID, productID)

	var specs string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT entitlements_spec_snapshot::text FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&specs))
	require.Contains(t, specs, "plan_b_access", "entitlement snapshot must cut over to the target product")
	require.NotContains(t, specs, "plan_a_access")

	// The reprice row is applied.
	rows, err := f.repriceRepo.List(ctx, SubscriptionRepriceFilter{RepriceBatchID: res.BatchID}, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, models.RepriceStatusApplied, rows[0].Status)
	require.Equal(t, models.RepriceKindPlanChange, rows[0].Kind)
}

// TestPlanMigration_FutureEffectiveDateDoesNotFlipEarly: a renewal BEFORE the
// effective date charges the old plan; the first renewal on/after flips.
func TestPlanMigration_FutureEffectiveDateDoesNotFlipEarly(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	_, railSubID := f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", true)

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID,
		EffectiveAt: f.clock.Now().Add(48 * time.Hour),
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Scheduled)

	f.clock.Advance(24 * time.Hour)
	amount := f.renewalAmount(t, ctx, models.RailNMI, railSubID)
	require.EqualValues(t, 10000000, amount, "renewal before effective_at keeps the old plan")

	f.clock.Advance(30 * time.Hour)
	amount = f.renewalAmount(t, ctx, models.RailNMI, railSubID)
	require.EqualValues(t, 9000000, amount, "first renewal on/after effective_at flips")
}

// TestPlanMigration_ImmediateOnEngineRail: Immediate cuts price/product/
// entitlement snapshots over NOW (no charge) and marks the row applied.
func TestPlanMigration_ImmediateOnEngineRail(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, _ := f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", true)

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID, Immediate: true})
	require.NoError(t, err)
	require.Equal(t, 1, res.Scheduled)
	require.Equal(t, "applied_immediately", res.Outcomes[0].Disposition)

	priceID, productID := f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.targetPriceID, priceID)
	require.Equal(t, f.targetProductID, productID)

	// No payment row was created by the flip.
	var payments int
	require.NoError(t, f.pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.payments p JOIN openrails.subscriptions s ON s.id = p.subscription_id
		WHERE s.id = $1`, subID).Scan(&payments))
	require.Zero(t, payments, "immediate migration must never charge")

	rows, err := f.repriceRepo.List(ctx, SubscriptionRepriceFilter{RepriceBatchID: res.BatchID}, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, models.RepriceStatusApplied, rows[0].Status)
}

// TestPlanMigration_StripeBoundaryPush: a stripe-native sub gets a Stripe
// subscription-schedule push (flip at period end); the row stays scheduled
// until converge observes the flip — simulated here via the converge hook
// query — and the internal cutover then follows provider truth.
func TestPlanMigration_StripeBoundaryPush(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscriptionOnRail(t, ctx, f.stripeSourcePriceID, "stripe", false)

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res.Scheduled)
	require.Len(t, f.stripe.schedules, 1, "boundary mode must push a Stripe schedule")
	require.Equal(t, railSubID, f.stripe.schedules[0]["subscription_id"])
	require.Contains(t, f.stripe.schedules[0]["current_price_id"], "price_src_")
	require.Contains(t, f.stripe.schedules[0]["new_price_id"], "price_tgt_")
	require.Empty(t, f.stripe.updates, "boundary mode must not update in place")

	// Row stays scheduled until provider truth flips.
	rows, err := f.repriceRepo.List(ctx, SubscriptionRepriceFilter{RepriceBatchID: res.BatchID}, 10, 0)
	require.NoError(t, err)
	require.Equal(t, models.RepriceStatusScheduled, rows[0].Status)

	// Converge hook: when the fetched price equals the target, the scheduled
	// row is marked applied (idempotent).
	n, err := f.dbi.Gen(ctx).ApplyScheduledRepriceForSubscriptionPrice(ctx, genApplyParams(subID, f.stripeTargetPriceID))
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	n, err = f.dbi.Gen(ctx).ApplyScheduledRepriceForSubscriptionPrice(ctx, genApplyParams(subID, f.stripeTargetPriceID))
	require.NoError(t, err)
	require.Zero(t, n, "second application is a no-op")
}

// TestPlanMigration_StripeImmediatePush: Immediate re-points the item now
// with proration_behavior=none and NO billing-anchor move.
func TestPlanMigration_StripeImmediatePush(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	_, railSubID := f.createSubscriptionOnRail(t, ctx, f.stripeSourcePriceID, "stripe", false)

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID, Immediate: true})
	require.NoError(t, err)
	require.Equal(t, 1, res.Scheduled)
	require.Len(t, f.stripe.updates, 1)
	u := f.stripe.updates[0]
	require.Equal(t, railSubID, u["subscription_id"])
	require.Equal(t, "si_fake", u["item_id"])
	require.Equal(t, "none", u["proration_behavior"], "forced migrations must never prorate")
	require.Empty(t, u["billing_anchor"], "forced migrations must never reset the cycle")
	require.Empty(t, f.stripe.schedules)
}

// TestPlanMigration_StripePushFailureDegradesToBlocked: a failed rail push
// leaves the sub on the old plan, records the row blocked with the push
// error, and keeps the batch going.
func TestPlanMigration_StripePushFailureDegradesToBlocked(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, _ := f.createSubscriptionOnRail(t, ctx, f.stripeSourcePriceID, "stripe", false)
	f.stripe.scheduleErr = fmt.Errorf("stripe 500")

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID})
	require.NoError(t, err)
	require.Equal(t, 0, res.Scheduled)
	require.Equal(t, 1, res.Blocked)
	require.Equal(t, "blocked", res.Outcomes[0].Disposition)
	require.Contains(t, res.Outcomes[0].Reason, "rail_push_failed")

	priceID, _ := f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.stripeSourcePriceID, priceID, "failed push leaves the sub untouched")

	rows, err := f.repriceRepo.List(ctx, SubscriptionRepriceFilter{RepriceBatchID: res.BatchID}, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, models.RepriceStatusBlocked, rows[0].Status)
	require.Contains(t, rows[0].BlockedReason, "stripe 500")

	// The persisted batch header must agree with its rows after degradation.
	batch, _, err := f.pm.GetBatch(ctx, *res.BatchID, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 0, batch.SubscriptionsScheduled)
	require.EqualValues(t, 1, batch.SubscriptionsBlocked)
}

// TestPlanMigration_CapabilityClassification: ccbill/solana land blocked
// rail_requires_user_action; gateway-native NMI recurring (no stored-
// credential anchor) is AUTO since #815 (server-side update_subscription);
// the preview reports per-rail counts.
func TestPlanMigration_CapabilityClassification(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "ccbill", false)
	f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "solana", false)
	// NMI WITHOUT a payment method (gateway-native recurring): AUTO via the
	// #815 pusher.
	f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", false)
	// Engine-anchored NMI: auto via the renewal-boundary pickup.
	f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", true)

	// Real PM lookup for this test: resolve from the DB so the anchored/
	// unanchored split is driven by actual rows.
	svc := NewPlanMigrationService(f.repriceSvc, f.stripe, f.nmi, pmLookupFunc(func(ctx context.Context, id uuid.UUID) (*models.PaymentMethod, error) {
		var anchor string
		if err := f.pool.QueryRow(ctx, `SELECT stored_credential_recurring_ref FROM openrails.payment_methods WHERE id = $1`, id).Scan(&anchor); err != nil {
			return nil, err
		}
		return &models.PaymentMethod{ID: id, StoredCredentialRecurringRef: anchor}, nil
	}))

	preview, err := svc.Preview(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 4, preview.Matched)
	require.Equal(t, 2, preview.Scheduled)
	require.Equal(t, 2, preview.Blocked)
	require.Nil(t, preview.BatchID, "preview must not create a batch")
	require.Equal(t, 1, preview.ByRail["ccbill"].RequiresAction)
	require.Equal(t, 1, preview.ByRail["solana"].RequiresAction)
	require.Equal(t, 2, preview.ByRail["nmi"].Auto, "anchored AND gateway-native NMI are both auto since #815")
	require.Empty(t, f.nmi.pushes, "preview must not push")

	// Preview wrote nothing.
	var batches, reprices int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM openrails.reprice_batches WHERE merchant_id = $1 AND kind = 'plan_change'`, f.merchantID).Scan(&batches))
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM openrails.subscription_reprices WHERE merchant_id = $1 AND kind = 'plan_change'`, f.merchantID).Scan(&reprices))
	require.Zero(t, batches)
	require.Zero(t, reprices)

	// Migrate records the blocked ledger rows and pushes the gateway-native
	// NMI sub (preview counts == execute classification).
	res, err := svc.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 2, res.Blocked)
	require.Equal(t, preview.Scheduled, res.Scheduled, "preview counts must match execute classification")
	require.Len(t, f.nmi.pushes, 1, "exactly the gateway-native NMI sub is pushed")
	require.EqualValues(t, 9000000, f.nmi.pushes[0]["amount_micros"])
	rows, err := f.repriceRepo.List(ctx, SubscriptionRepriceFilter{RepriceBatchID: res.BatchID}, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	blocked := 0
	for _, row := range rows {
		if row.Status == models.RepriceStatusBlocked {
			blocked++
			require.Equal(t, "rail_requires_user_action", row.BlockedReason)
		}
	}
	require.Equal(t, 2, blocked)
}

// TestPlanMigration_NoticeWindowGate (#781 reuse): an INCREASE migration
// inside the merchant's notice window is refused per-sub without the
// explicit acknowledgement, and scheduled with it.
func TestPlanMigration_NoticeWindowGate(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	f.setNoticeWindowDays(t, ctx, 30)
	// stripeTarget (14M) > low (10M): an increase.
	f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", true)

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.stripeTargetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res.Skipped)
	require.Equal(t, 0, res.Scheduled)
	require.Contains(t, res.Outcomes[0].Reason, "notice window")

	res, err = f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.stripeTargetPriceID, AcknowledgeShortNotice: true})
	require.NoError(t, err)
	require.Equal(t, 1, res.Scheduled)
}

// TestPlanMigration_Validation: same-product, cross-currency, archived
// target, bad fallback all refuse up front.
func TestPlanMigration_Validation(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	_, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.highPriceID})
	require.ErrorIs(t, err, ErrPlanMigrationSameProduct)

	// eur source (on product A) -> usd target (on product B): cross-currency.
	_, err = f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.otherCurrencyPriceID, TargetPriceID: f.targetPriceID})
	require.ErrorIs(t, err, ErrRepriceCrossCurrency)

	// Cross-currency: source usd (low), target eur on other product.
	eurTargetID := uuid.New()
	_, e := f.pool.Exec(ctx, `
		INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, access_duration_hours, auto_renew, archived, key, created_at, updated_at)
		VALUES ($1,$2,$3,9000000,'EUR',720,true,false,$4,$5,$5)`,
		eurTargetID, f.targetProductID, f.merchantID, "planmig-eur-"+uuid.NewString()[:8], f.clock.Now())
	require.NoError(t, e)
	_, err = f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: eurTargetID})
	require.ErrorIs(t, err, ErrRepriceCrossCurrency)

	// Archived target.
	require.NoError(t, f.priceSvc.SetArchived(ctx, f.targetPriceID, true))
	_, err = f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.ErrorIs(t, err, ErrRepriceInactivePrice)
	require.NoError(t, f.priceSvc.SetArchived(ctx, f.targetPriceID, false))

	// Bad fallback.
	_, err = f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID, FallbackPolicy: "nuke"})
	require.ErrorIs(t, err, ErrPlanMigrationBadFallback)
}

// TestPlanMigration_IdempotentRerun: re-running the migration skips already-
// scheduled subs (one-scheduled conflict) and already-migrated subs (already
// on target).
func TestPlanMigration_IdempotentRerun(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", true)

	res1, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res1.Scheduled)

	res2, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 0, res2.Scheduled)
	require.Equal(t, 1, res2.Skipped, "second run skips via the one-scheduled conflict")

	// After an Immediate run on a second sub, a further re-run skips it as
	// already on target.
	sub2, _ := f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", true)
	res3, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID, Immediate: true})
	require.NoError(t, err)
	require.Equal(t, 1, res3.Scheduled)
	priceID, _ := f.subscriptionRow(t, ctx, sub2)
	require.Equal(t, f.targetPriceID, priceID)
	res4, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Zero(t, res4.Scheduled)
}

// TestPlanMigration_CancelBatch: cancel-before-effective cancels every
// still-scheduled row; the next renewal charges the OLD plan.
func TestPlanMigration_CancelBatch(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	_, railSubID := f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", true)

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID, EffectiveAt: f.clock.Now().Add(24 * time.Hour)})
	require.NoError(t, err)
	require.Equal(t, 1, res.Scheduled)

	cres, err := f.pm.CancelBatch(ctx, *res.BatchID)
	require.NoError(t, err)
	require.Equal(t, 1, cres.Canceled)
	require.Empty(t, cres.RailReleaseRequired, "engine-rail cancel is complete on both sides")
	require.Empty(t, cres.Warning)

	f.clock.Advance(48 * time.Hour)
	amount := f.renewalAmount(t, ctx, models.RailNMI, railSubID)
	require.EqualValues(t, 10000000, amount, "canceled migration must not flip")

	batch, rows, err := f.pm.GetBatch(ctx, *res.BatchID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, models.RepriceKindPlanChange, batch.Kind)
	require.Len(t, rows, 1)
	require.Equal(t, models.RepriceStatusCanceled, rows[0].Status)
}

// TestPlanMigration_CancelAfterStripePushWarnsLoudly: cancelling a batch
// whose Stripe boundary push already planted a provider-side schedule must
// surface the divergence — Stripe will still flip at period end unless the
// schedule is released out of band.
func TestPlanMigration_CancelAfterStripePushWarnsLoudly(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, _ := f.createSubscriptionOnRail(t, ctx, f.stripeSourcePriceID, "stripe", false)

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res.Scheduled)
	require.Len(t, f.stripe.schedules, 1)

	cres, err := f.pm.CancelBatch(ctx, *res.BatchID)
	require.NoError(t, err)
	require.Equal(t, 1, cres.Canceled)
	require.Equal(t, []uuid.UUID{subID}, cres.RailReleaseRequired)
	require.Contains(t, cres.Warning, "WILL flip the price at period end")
}

// TestPlanMigration_SkipsSelfScheduledDowngrade: a subscription with a
// pending self-serve TierChange downgrade (ScheduledPriceID set) is already
// leaving the source price — stacking a plan-change row on the same renewal
// boundary would double-schedule; it must be skipped with a reason.
func TestPlanMigration_SkipsSelfScheduledDowngrade(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, _ := f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", true)

	sub, err := f.subSvc.GetByID(ctx, subID)
	require.NoError(t, err)
	sub.ScheduledPriceID = &f.stripeTargetPriceID
	require.NoError(t, err)
	require.NoError(t, f.subSvc.Update(ctx, sub))

	res, err := f.pm.Preview(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res.Matched)
	require.Zero(t, res.Scheduled)
	require.Equal(t, 1, res.Skipped)
	require.Equal(t, "self-scheduled downgrade pending", res.Outcomes[0].Reason)
}

// TestPlanMigration_StripeFarFutureEffectiveBlocksHonestly: pushing a Stripe
// schedule for an effective date beyond the current period would flip a whole
// period EARLY — the row must block instead, and a re-run inside the final
// pre-effective period must succeed.
func TestPlanMigration_StripeFarFutureEffectiveBlocksHonestly(t *testing.T) {
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, _ := f.createSubscriptionOnRail(t, ctx, f.stripeSourcePriceID, "stripe", false)

	// Period ends +30d; effective +45d — beyond it.
	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID,
		EffectiveAt: f.clock.Now().Add(45 * 24 * time.Hour),
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Blocked)
	require.Contains(t, res.Outcomes[0].Reason, "stripe_deferred_push_required")
	require.Empty(t, f.stripe.schedules, "no early flip may be pushed")

	// Simulate the sub renewing into its final pre-effective period
	// (period now ends +60d > effective +45d): re-run succeeds.
	_, err = f.pool.Exec(ctx, `UPDATE openrails.subscriptions SET current_period_ends_at = $1 WHERE id = $2`,
		f.clock.Now().Add(60*24*time.Hour), subID)
	require.NoError(t, err)
	res2, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.stripeSourcePriceID, TargetPriceID: f.stripeTargetPriceID,
		EffectiveAt: f.clock.Now().Add(45 * 24 * time.Hour),
	})
	require.NoError(t, err)
	require.Equal(t, 1, res2.Scheduled)
	require.Len(t, f.stripe.schedules, 1)
}
