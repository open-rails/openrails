//go:build integration

package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// repriceFixture provisions one product with two co-active, same-currency
// prices (low/high) for reprice scenarios, plus cross-product/cross-currency/
// inactive targets for the constraint-violation tests.
type repriceFixture struct {
	dbi         *db.DB
	pool        *pgxpool.Pool
	clock       *clockwork.FakeClock
	priceSvc    *catalog.PriceService
	subSvc      *SubscriptionService
	lifecycle   *SubscriptionLifecycleService
	repriceRepo *RepriceRepo
	repriceSvc  *RepriceService
	config      *merchantconfig.Store

	merchantID              uuid.UUID
	productID               uuid.UUID
	lowPriceID, highPriceID uuid.UUID
	otherProductPriceID     uuid.UUID
	otherCurrencyPriceID    uuid.UUID
	inactivePriceID         uuid.UUID
}

func newRepriceFixture(t *testing.T) *repriceFixture {
	t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	merchantID := dbtest.TestMerchantID.UUID()

	// Start comfortably in the past: payments.chk_payment_not_future checks
	// purchased_at against Postgres' REAL now(), so a fake clock advanced
	// forward through the test must never cross real wall-clock time.
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Add(-96 * time.Hour))
	now := clock.Now()

	productID := uuid.New()
	otherProductID := uuid.New()
	suffix := uuid.NewString()[:8]
	_, err := pool.Exec(ctx, `INSERT INTO openrails.products (id, merchant_id, key, display_name) VALUES ($1,$2,$3,$3)`,
		productID, merchantID, "reprice-product-"+suffix)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id, merchant_id, key, display_name) VALUES ($1,$2,$3,$3)`,
		otherProductID, merchantID, "reprice-other-product-"+suffix)
	require.NoError(t, err)

	insertPrice := func(id, productID uuid.UUID, amount int64, currency string, archived bool, key string) {
		_, e := pool.Exec(ctx, `
			INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, access_duration_hours, auto_renew, archived, key, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,720,true,$6,$7,$8,$8)`,
			id, productID, merchantID, amount, currency, archived, key, now)
		require.NoError(t, e)
	}
	lowPriceID, highPriceID := uuid.New(), uuid.New()
	otherProductPriceID := uuid.New()
	otherCurrencyPriceID := uuid.New()
	inactivePriceID := uuid.New()
	insertPrice(lowPriceID, productID, 10000000, "USD", false, "reprice-low-"+suffix)
	insertPrice(highPriceID, productID, 12000000, "USD", false, "reprice-high-"+suffix)
	insertPrice(otherProductPriceID, otherProductID, 10000000, "USD", false, "reprice-other-product-price-"+suffix)
	insertPrice(otherCurrencyPriceID, productID, 10000000, "EUR", false, "reprice-eur-"+suffix)
	insertPrice(inactivePriceID, productID, 15000000, "USD", true, "reprice-inactive-"+suffix)

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, clock)
	subSvc := NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil, clock)
	lifecycle := NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, clock)
	repriceRepo := NewRepriceRepo(dbi)
	configStore := merchantconfig.NewStore(dbi)
	// #781: this fixture's low/high prices exist to test renewal-boundary,
	// cancel, and constraint mechanics — NOT notice-window semantics — so
	// disable the gate (an explicit merchant override of 0 days) here rather
	// than making every existing case pick a 30-day-compliant effective_at
	// (which would fight the fake clock's real-wall-clock ceiling, see above).
	// The dedicated notice-window tests set their own window explicitly.
	zeroWindow := 0
	require.NoError(t, configStore.Upsert(ctx, models.MerchantConfiguration{RepriceNoticeWindowDays: &zeroWindow}))
	repriceSvc := NewRepriceService(dbi, repriceRepo, priceSvc, subSvc, notifSvc, configStore, clock)

	f := &repriceFixture{
		dbi: dbi, pool: pool, clock: clock, priceSvc: priceSvc, subSvc: subSvc,
		lifecycle: lifecycle, repriceRepo: repriceRepo, repriceSvc: repriceSvc, config: configStore,
		merchantID: merchantID, productID: productID,
		lowPriceID: lowPriceID, highPriceID: highPriceID,
		otherProductPriceID: otherProductPriceID, otherCurrencyPriceID: otherCurrencyPriceID,
		inactivePriceID: inactivePriceID,
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.subscription_reprices WHERE merchant_id = $1", merchantID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.reprice_batches WHERE merchant_id = $1", merchantID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.payments WHERE price_id = ANY($1)",
			[]uuid.UUID{lowPriceID, highPriceID, otherProductPriceID, otherCurrencyPriceID, inactivePriceID})
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.subscriptions WHERE product_id IN ($1,$2)", productID, otherProductID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.prices WHERE product_id IN ($1,$2)", productID, otherProductID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.products WHERE id IN ($1,$2)", productID, otherProductID)
	})
	return f
}

// setNoticeWindowDays (#781) overrides this fixture's merchant-configured
// reprice notice window (the fixture defaults to 0 — disabled — so every
// pre-existing non-notice-window test is unaffected; tests exercising the
// window call this explicitly).
func (f *repriceFixture) setNoticeWindowDays(t *testing.T, ctx context.Context, days int) {
	t.Helper()
	require.NoError(t, f.config.Upsert(ctx, models.MerchantConfiguration{RepriceNoticeWindowDays: &days}))
}

// createSubscription seeds an active subscription pinned to priceID, billing
// on the "nmi" rail under a fresh rail_subscription_id. product_id is
// resolved from priceID itself so cross-product fixtures wire correctly.
func (f *repriceFixture) createSubscription(t *testing.T, ctx context.Context, priceID uuid.UUID) (subID uuid.UUID, railSubID string) {
	t.Helper()
	var productID uuid.UUID
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT product_id FROM openrails.prices WHERE id = $1`, priceID).Scan(&productID))

	subject := "reprice-customer-" + uuid.NewString()
	var customerID uuid.UUID
	require.NoError(t, f.pool.QueryRow(ctx,
		`INSERT INTO openrails.customers (merchant_id, subject) VALUES ($1,$2) RETURNING id`,
		f.merchantID, subject).Scan(&customerID))

	railSubID = "reprice-rail-sub-" + uuid.NewString()
	now := f.clock.Now()
	periodEnd := now.Add(30 * 24 * time.Hour)
	require.NoError(t, f.pool.QueryRow(ctx, `
		INSERT INTO openrails.subscriptions (merchant_id, customer_id, product_id, price_id, status, rail, rail_subscription_id, current_period_starts_at, current_period_ends_at)
		VALUES ($1,$2,$3,$4,'active','nmi',$5,$6,$7) RETURNING id`,
		f.merchantID, customerID, productID, priceID, railSubID, now, periodEnd).Scan(&subID))
	return subID, railSubID
}

func (f *repriceFixture) renewalAmount(t *testing.T, ctx context.Context, rail models.Rail, railSubID string) int64 {
	t.Helper()
	require.NoError(t, f.lifecycle.RenewMembership(ctx, &RenewMembershipParams{
		Rail:               rail,
		RailSubscriptionID: railSubID,
		TransactionID:      "reprice-txn-" + uuid.NewString(),
	}))
	var amount int64
	require.NoError(t, f.pool.QueryRow(ctx, `
		SELECT p.amount FROM openrails.payments p
		JOIN openrails.subscriptions s ON s.id = p.subscription_id
		WHERE s.rail_subscription_id = $1
		ORDER BY p.created_at DESC LIMIT 1`, railSubID).Scan(&amount))
	return amount
}

// TestReprice_ScheduledIncreaseFlipsAtFirstRenewalOnOrAfterEffectiveDate is
// #773's core mechanic: a reprice scheduled for a FUTURE effective date does
// NOT apply at a renewal before that date, but DOES apply — flipping the
// charged amount — at the first renewal on/after it.
func TestReprice_ScheduledIncreaseFlipsAtFirstRenewalOnOrAfterEffectiveDate(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscription(t, ctx, f.lowPriceID)

	effectiveAt := f.clock.Now().Add(48 * time.Hour)
	rr, err := f.repriceSvc.Reprice(ctx, RepriceRequest{SubscriptionID: subID, ToPriceID: f.highPriceID, EffectiveAt: effectiveAt})
	require.NoError(t, err)
	require.Equal(t, models.RepriceStatusScheduled, rr.Status)

	// A renewal BEFORE the effective date still charges the OLD (low) amount.
	f.clock.Advance(24 * time.Hour)
	amount := f.renewalAmount(t, ctx, models.RailNMI, railSubID)
	require.EqualValues(t, 10000000, amount, "renewal before effective_at must not flip")

	scheduled, err := f.repriceRepo.GetScheduledForSubscription(ctx, subID)
	require.NoError(t, err, "still scheduled — not yet due")
	require.Equal(t, rr.ID, scheduled.ID)

	// A SECOND renewal on/after the effective date flips to the NEW (high)
	// amount and marks the reprice applied.
	f.clock.Advance(30 * time.Hour) // now +54h > +48h effective_at
	amount = f.renewalAmount(t, ctx, models.RailNMI, railSubID)
	require.EqualValues(t, 12000000, amount, "first renewal on/after effective_at must flip")

	_, err = f.repriceRepo.GetScheduledForSubscription(ctx, subID)
	require.ErrorIs(t, err, pgx.ErrNoRows, "no longer scheduled once applied")

	applied, err := f.repriceRepo.GetByID(ctx, rr.ID)
	require.NoError(t, err)
	require.Equal(t, models.RepriceStatusApplied, applied.Status)
	require.NotNil(t, applied.AppliedAt)

	sub, err := f.subSvc.GetByID(ctx, subID)
	require.NoError(t, err)
	require.Equal(t, f.highPriceID, sub.PriceID, "subscription re-pinned to the new price")
}

// TestReprice_DecreaseEffectiveNow_FlipsAtNextRenewal covers a decrease with
// effective_at=now: it still only takes effect at the subscription's actual
// next renewal (v1: no instant/mid-cycle application), never before.
func TestReprice_DecreaseEffectiveNow_FlipsAtNextRenewal(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscription(t, ctx, f.highPriceID)

	_, err := f.repriceSvc.Reprice(ctx, RepriceRequest{SubscriptionID: subID, ToPriceID: f.lowPriceID, EffectiveAt: f.clock.Now()})
	require.NoError(t, err)

	amount := f.renewalAmount(t, ctx, models.RailNMI, railSubID)
	require.EqualValues(t, 10000000, amount, "decrease effective now flips at the next renewal")
}

// TestReprice_CancelBeforeEffective_LeavesSubscriptionUntouched: canceling a
// scheduled reprice before its effective date means the next renewal is
// completely unaffected.
func TestReprice_CancelBeforeEffective_LeavesSubscriptionUntouched(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscription(t, ctx, f.lowPriceID)

	rr, err := f.repriceSvc.Reprice(ctx, RepriceRequest{SubscriptionID: subID, ToPriceID: f.highPriceID, EffectiveAt: f.clock.Now().Add(24 * time.Hour)})
	require.NoError(t, err)
	require.NoError(t, f.repriceSvc.Cancel(ctx, rr.ID))

	f.clock.Advance(48 * time.Hour) // well past the (now-canceled) effective date
	amount := f.renewalAmount(t, ctx, models.RailNMI, railSubID)
	require.EqualValues(t, 10000000, amount, "a canceled reprice must never apply")

	canceled, err := f.repriceRepo.GetByID(ctx, rr.ID)
	require.NoError(t, err)
	require.Equal(t, models.RepriceStatusCanceled, canceled.Status)
	require.NotNil(t, canceled.CanceledAt)
}

// TestReprice_AlreadyAppliedStaysAppliedAfterCancelAttempt: once a reprice has
// applied, a (late) cancel attempt is refused and the subscription stays on
// the new price — an already-flipped subscription is never unflipped.
func TestReprice_AlreadyAppliedStaysAppliedAfterCancelAttempt(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscription(t, ctx, f.lowPriceID)

	rr, err := f.repriceSvc.Reprice(ctx, RepriceRequest{SubscriptionID: subID, ToPriceID: f.highPriceID, EffectiveAt: f.clock.Now()})
	require.NoError(t, err)

	amount := f.renewalAmount(t, ctx, models.RailNMI, railSubID)
	require.EqualValues(t, 12000000, amount)

	err = f.repriceSvc.Cancel(ctx, rr.ID)
	require.ErrorIs(t, err, ErrRepriceNotScheduled)

	sub, err := f.subSvc.GetByID(ctx, subID)
	require.NoError(t, err)
	require.Equal(t, f.highPriceID, sub.PriceID, "already-flipped subscription stays flipped after a failed cancel")
}

// TestReprice_AlreadyScheduled_Refused: at most one scheduled reprice may
// exist per subscription at a time.
func TestReprice_AlreadyScheduled_Refused(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, _ := f.createSubscription(t, ctx, f.lowPriceID)

	_, err := f.repriceSvc.Reprice(ctx, RepriceRequest{SubscriptionID: subID, ToPriceID: f.highPriceID, EffectiveAt: f.clock.Now().Add(time.Hour)})
	require.NoError(t, err)

	_, err = f.repriceSvc.Reprice(ctx, RepriceRequest{SubscriptionID: subID, ToPriceID: f.highPriceID, EffectiveAt: f.clock.Now().Add(2 * time.Hour)})
	require.ErrorIs(t, err, ErrRepriceAlreadyScheduled)
}

// TestReprice_ConstraintViolations_TypedErrors: cross-product, cross-currency,
// and inactive targets are refused fail-closed with typed sentinels.
func TestReprice_ConstraintViolations_TypedErrors(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	cases := []struct {
		name    string
		toPrice uuid.UUID
		want    error
	}{
		{"cross_product", f.otherProductPriceID, ErrRepriceCrossProduct},
		{"cross_currency", f.otherCurrencyPriceID, ErrRepriceCrossCurrency},
		{"inactive", f.inactivePriceID, ErrRepriceInactivePrice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subID, _ := f.createSubscription(t, ctx, f.lowPriceID)
			_, err := f.repriceSvc.Reprice(ctx, RepriceRequest{SubscriptionID: subID, ToPriceID: tc.toPrice, EffectiveAt: f.clock.Now()})
			require.Error(t, err)
			require.ErrorIs(t, err, tc.want)
			var constraintErr *RepriceConstraintError
			require.ErrorAs(t, err, &constraintErr)
			require.Equal(t, subID, constraintErr.SubscriptionID)
		})
	}
}

// TestRepriceAllPriorVersions_BulkSchedulesOnlyPriorVersions: the bulk
// operation schedules every ACTIVE subscription pinned to an ARCHIVED prior
// version of the key, targeting the key's CURRENT price — grandfathered
// subscriptions on OTHER products/keys are untouched.
func TestRepriceAllPriorVersions_BulkSchedulesOnlyPriorVersions(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	key := "reprice-bulk-key-" + uuid.NewString()[:8]
	// Build a #774 version chain manually: lowPriceID was the original (now
	// archived), highPriceID is the current holder of `key`.
	require.NoError(t, f.priceSvc.SetKey(ctx, f.lowPriceID, key))
	require.NoError(t, f.priceSvc.SetArchived(ctx, f.lowPriceID, true))
	require.NoError(t, f.priceSvc.SetKey(ctx, f.highPriceID, key))
	require.NoError(t, f.priceSvc.RecordKeyMovement(ctx, f.merchantID, f.lowPriceID, key, f.clock.Now().Add(-time.Hour)))
	require.NoError(t, f.priceSvc.RecordKeyMovement(ctx, f.merchantID, f.highPriceID, key, f.clock.Now()))

	pinnedToPrior, _ := f.createSubscription(t, ctx, f.lowPriceID)
	unrelated, _ := f.createSubscription(t, ctx, f.otherProductPriceID)

	effectiveAt := f.clock.Now().Add(24 * time.Hour)
	result, err := f.repriceSvc.RepriceAllPriorVersions(ctx, RepriceAllPriorVersionsRequest{PriceKey: key, EffectiveAt: effectiveAt})
	require.NoError(t, err)
	require.Equal(t, f.highPriceID, result.ToPriceID)
	require.Equal(t, 1, result.Matched)
	require.Len(t, result.Scheduled, 1)
	require.Equal(t, pinnedToPrior, result.Scheduled[0].SubscriptionID)

	scheduled, err := f.repriceRepo.GetScheduledForSubscription(ctx, pinnedToPrior)
	require.NoError(t, err)
	require.Equal(t, f.highPriceID, scheduled.ToPriceID)
	require.WithinDuration(t, effectiveAt, scheduled.EffectiveAt.UTC(), time.Microsecond, "postgres timestamptz rounds to microsecond precision")

	_, err = f.repriceRepo.GetScheduledForSubscription(ctx, unrelated)
	require.ErrorIs(t, err, pgx.ErrNoRows, "a subscription pinned to a DIFFERENT key/product must not be touched")
}

// TestReprice_NoticeWindow_IncreaseInsideWindow_Refused (#781): an INCREASE
// whose effective_at is nearer than the merchant's configured notice window
// is refused with a typed constraint error — the server-side gate #773 never
// had (proven, pre-#781, by the wizard's own integration test accepting a
// 1-day-out increase without complaint).
func TestReprice_NoticeWindow_IncreaseInsideWindow_Refused(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	f.setNoticeWindowDays(t, ctx, 30)
	subID, _ := f.createSubscription(t, ctx, f.lowPriceID)

	_, err := f.repriceSvc.Reprice(ctx, RepriceRequest{
		SubscriptionID: subID, ToPriceID: f.highPriceID, EffectiveAt: f.clock.Now().Add(24 * time.Hour),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRepriceNoticeWindowViolation)
	var constraintErr *RepriceConstraintError
	require.ErrorAs(t, err, &constraintErr)
	require.Equal(t, subID, constraintErr.SubscriptionID)

	// Fail-closed: nothing was scheduled.
	_, err = f.repriceRepo.GetScheduledForSubscription(ctx, subID)
	require.ErrorIs(t, err, pgx.ErrNoRows)
}

// TestReprice_NoticeWindow_DecreaseExempt_EvenOneDayOut: decreases never need
// advance notice, regardless of the configured window.
func TestReprice_NoticeWindow_DecreaseExempt_EvenOneDayOut(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	f.setNoticeWindowDays(t, ctx, 30)
	subID, _ := f.createSubscription(t, ctx, f.highPriceID)

	rr, err := f.repriceSvc.Reprice(ctx, RepriceRequest{
		SubscriptionID: subID, ToPriceID: f.lowPriceID, EffectiveAt: f.clock.Now().Add(24 * time.Hour),
	})
	require.NoError(t, err)
	require.Equal(t, models.RepriceStatusScheduled, rr.Status)
	require.False(t, rr.AcknowledgedShortNotice, "a decrease never needs the override")
}

// TestReprice_NoticeWindow_AcknowledgeShortNotice_OverridesAndAudits: the
// explicit support/emergency escape hatch schedules the increase anyway and
// leaves audit evidence on the persisted row — never silent.
func TestReprice_NoticeWindow_AcknowledgeShortNotice_OverridesAndAudits(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	f.setNoticeWindowDays(t, ctx, 30)
	subID, _ := f.createSubscription(t, ctx, f.lowPriceID)

	rr, err := f.repriceSvc.Reprice(ctx, RepriceRequest{
		SubscriptionID: subID, ToPriceID: f.highPriceID, EffectiveAt: f.clock.Now().Add(24 * time.Hour),
		AcknowledgeShortNotice: true,
	})
	require.NoError(t, err)
	require.Equal(t, models.RepriceStatusScheduled, rr.Status)
	require.True(t, rr.AcknowledgedShortNotice, "the override must be recorded on the row, never silent")

	// Re-fetch independently — the audit trail is durable, not just an
	// in-memory echo of the request.
	fetched, err := f.repriceRepo.GetByID(ctx, rr.ID)
	require.NoError(t, err)
	require.True(t, fetched.AcknowledgedShortNotice)
}

// TestReprice_NoticeWindow_MerchantConfiguredWindowRespected: a merchant that
// configures a SHORTER window than the default gets that shorter window
// enforced, with no override needed.
func TestReprice_NoticeWindow_MerchantConfiguredWindowRespected(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	f.setNoticeWindowDays(t, ctx, 2)
	subID, _ := f.createSubscription(t, ctx, f.lowPriceID)

	// 3 days out would violate the DEFAULT (30d) window but satisfies this
	// merchant's configured 2-day window.
	rr, err := f.repriceSvc.Reprice(ctx, RepriceRequest{
		SubscriptionID: subID, ToPriceID: f.highPriceID, EffectiveAt: f.clock.Now().Add(3 * 24 * time.Hour),
	})
	require.NoError(t, err)
	require.False(t, rr.AcknowledgedShortNotice, "no override needed — the configured window is satisfied")

	// Tightening the window back up refuses a second subscription at the same
	// (now non-compliant) notice.
	f.setNoticeWindowDays(t, ctx, 10)
	subID2, _ := f.createSubscription(t, ctx, f.lowPriceID)
	_, err = f.repriceSvc.Reprice(ctx, RepriceRequest{
		SubscriptionID: subID2, ToPriceID: f.highPriceID, EffectiveAt: f.clock.Now().Add(3 * 24 * time.Hour),
	})
	require.ErrorIs(t, err, ErrRepriceNoticeWindowViolation)
}

// TestRepriceAllPriorVersions_NoticeWindow_IncreaseInsideWindowSkipped: the
// bulk operation's skip-not-abort semantics apply to the notice-window
// constraint exactly like the others (cross-product/currency/inactive) —
// matched but skipped, with a typed reason, never silently scheduled early.
func TestRepriceAllPriorVersions_NoticeWindow_IncreaseInsideWindowSkipped(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	f.setNoticeWindowDays(t, ctx, 30)

	key := "reprice-notice-bulk-key-" + uuid.NewString()[:8]
	require.NoError(t, f.priceSvc.SetKey(ctx, f.lowPriceID, key))
	require.NoError(t, f.priceSvc.SetArchived(ctx, f.lowPriceID, true))
	require.NoError(t, f.priceSvc.SetKey(ctx, f.highPriceID, key))
	require.NoError(t, f.priceSvc.RecordKeyMovement(ctx, f.merchantID, f.lowPriceID, key, f.clock.Now().Add(-time.Hour)))
	require.NoError(t, f.priceSvc.RecordKeyMovement(ctx, f.merchantID, f.highPriceID, key, f.clock.Now()))

	pinned, _ := f.createSubscription(t, ctx, f.lowPriceID)

	result, err := f.repriceSvc.RepriceAllPriorVersions(ctx, RepriceAllPriorVersionsRequest{
		PriceKey: key, EffectiveAt: f.clock.Now().Add(24 * time.Hour),
	})
	require.NoError(t, err, "bulk never aborts on a per-subscription constraint violation")
	require.Equal(t, 1, result.Matched)
	require.Empty(t, result.Scheduled)
	require.Len(t, result.Skipped, 1)
	require.Equal(t, pinned, result.Skipped[0].SubscriptionID)
	require.Contains(t, result.Skipped[0].Reason, "notice window")

	_, err = f.repriceRepo.GetScheduledForSubscription(ctx, pinned)
	require.ErrorIs(t, err, pgx.ErrNoRows, "skipped, never scheduled")
}

// TestRepriceAllPriorVersions_NoticeWindow_AcknowledgeShortNotice: the bulk
// override schedules the whole batch anyway, with audit evidence on both the
// API response and the persisted rows.
func TestRepriceAllPriorVersions_NoticeWindow_AcknowledgeShortNotice(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	f.setNoticeWindowDays(t, ctx, 30)

	key := "reprice-notice-bulk-ack-key-" + uuid.NewString()[:8]
	require.NoError(t, f.priceSvc.SetKey(ctx, f.lowPriceID, key))
	require.NoError(t, f.priceSvc.SetArchived(ctx, f.lowPriceID, true))
	require.NoError(t, f.priceSvc.SetKey(ctx, f.highPriceID, key))
	require.NoError(t, f.priceSvc.RecordKeyMovement(ctx, f.merchantID, f.lowPriceID, key, f.clock.Now().Add(-time.Hour)))
	require.NoError(t, f.priceSvc.RecordKeyMovement(ctx, f.merchantID, f.highPriceID, key, f.clock.Now()))

	pinned, _ := f.createSubscription(t, ctx, f.lowPriceID)

	result, err := f.repriceSvc.RepriceAllPriorVersions(ctx, RepriceAllPriorVersionsRequest{
		PriceKey: key, EffectiveAt: f.clock.Now().Add(24 * time.Hour), AcknowledgeShortNotice: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Matched)
	require.Empty(t, result.Skipped)
	require.Len(t, result.Scheduled, 1)
	require.Equal(t, pinned, result.Scheduled[0].SubscriptionID)
	require.True(t, result.Scheduled[0].AcknowledgedShortNotice, "audit evidence on the API response")

	scheduled, err := f.repriceRepo.GetScheduledForSubscription(ctx, pinned)
	require.NoError(t, err)
	require.True(t, scheduled.AcknowledgedShortNotice, "audit evidence durably persisted on the row")
}

// fakeCharger captures the last charge.Request it received, so tests can
// assert the stored-credential anchor (Context.PriorRef) stays identical
// across a reprice while only the amount changes.
type fakeCharger struct {
	lastReq charge.Request
	result  charge.Result
	err     error
}

func (c *fakeCharger) Charge(_ context.Context, req charge.Request) (charge.Result, error) {
	c.lastReq = req
	if c.err != nil {
		return charge.Result{}, c.err
	}
	return c.result, nil
}
