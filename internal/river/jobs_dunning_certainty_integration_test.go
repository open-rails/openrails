//go:build integration

package riverjobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #839/#840: the destructive-action audit's two go-live blockers. Both let the
// unattended dunning worker reach a TERMINAL outcome — local cancel +
// entitlement revoke + a queued, IRREVERSIBLE NMI vault delete — on evidence
// that is not evidence:
//
//	#839  a DATE COMPARISON (now > period_end + window), before any charge
//	      attempt. A sub-4-day cycle derived a ZERO window, so the comparison
//	      was true by construction and a daily subscription was destroyed on
//	      its first dunning touch, never having been billed once.
//	#840  the ABSENCE OF ONE OF OUR OWN ROWS (payment_methods vault refs), with
//	      no forensic payments row written. It counted as a dunning failure and
//	      DunningMaxFailures is 1 for short cycles, so the first observation was
//	      terminal. A vault-sync bug was indistinguishable from a dead card.
//
// Both must PARK as `unknown` instead: access intact, out of the dunning queue,
// resolved by the unknown-cohort provider probe. And #836's kill switch must be
// able to stop the one remaining terminal path (a real provider decline).

// dunningCertaintyFixture is one merchant + product + price + subscription
// shaped for a dunning pass, with the deferred NMI-delete scheduler wired to
// the REAL intent ledger so "did this queue a vault delete?" is answered by the
// production mechanism rather than a stub.
type dunningCertaintyFixture struct {
	dbi        *db.DB
	ctx        context.Context
	worker     *DunningWorker
	lifecycle  *subscriptions.SubscriptionLifecycleService
	priceSvc   *catalog.PriceService
	moneySvc   *money.MoneyService
	subSvc     *subscriptions.SubscriptionService
	subID      uuid.UUID
	nmiWrites  *atomic.Int64
	nmiRespond func(w http.ResponseWriter)
}

// newDunningCertaintyFixture seeds a past_due NMI subscription on a price whose
// billing cycle is cycleHours, with its period ending periodEndAgo in the past.
// vaultRefs=false leaves the local payment-method vault refs EMPTY (#840).
func newDunningCertaintyFixture(t *testing.T, cycleHours int32, periodEndAgo time.Duration, vaultRefs bool) *dunningCertaintyFixture {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	baseCtx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(baseCtx, t,
		dbtest.OpenAppDB(t, dbtest.SharedSuperuserDSN(t)).Pool())
	merchantID := dbtest.TestMerchantID.UUID()
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID, subID, pmID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	description := "Certainty"
	var customerID uuid.UUID
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		q := dbi.Gen(ctx)
		_, err := q.CreateProduct(ctx, gen.CreateProductParams{
			ID: productID, Key: "certainty_product_" + uuid.New().String(), DisplayName: "Certainty Product",
			MerchantID: merchantID, Description: &description, Archived: false, CreatedAt: now, UpdatedAt: now,
		})
		require.NoError(t, err)
		_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
			ID: priceID, ProductID: productID, Amount: 999, Currency: "USD", MerchantID: merchantID,
			Archived: false, AccessDurationHours: &cycleHours, AutoRenew: true, CreatedAt: now, UpdatedAt: now,
		})
		require.NoError(t, err)

		customerID = dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Qx(ctx), uuid.New().String())
		customerRef, methodRef := "", ""
		if vaultRefs {
			customerRef, methodRef = "vault_"+uuid.New().String(), "card_"+uuid.New().String()
		}
		_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
			ID: pmID, MerchantID: merchantID, CustomerID: customerID, Rail: string(models.RailNMI),
			RailCustomerRef: customerRef, RailMethodRef: methodRef,
			// RebillDriver=openrails: WE drive the rebill, so this is emphatically
			// not the #635 vault-less provider-auto-billed shape.
			RebillDriver:         "openrails",
			InitialTransactionID: "txn_initial_" + uuid.New().String(), CreatedAt: now, UpdatedAt: now,
		})
		require.NoError(t, err)

		periodEnd := now.Add(-periodEndAgo)
		periodStart := periodEnd.Add(-time.Duration(cycleHours) * time.Hour)
		nextRetry := now.Add(-time.Minute)
		_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
			ID: subID, MerchantID: merchantID, CustomerID: customerID, ProductID: productID, PriceID: &priceID,
			Status: string(models.StatusPastDue), Rail: string(models.RailNMI),
			RailSubscriptionID: "sub_certainty_" + uuid.New().String(), PaymentMethodID: &pmID,
			CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
			NextRetryAt: &nextRetry, CreatedAt: now, UpdatedAt: now,
		})
		require.NoError(t, err)
		return nil
	}))

	t.Cleanup(func() {
		_ = dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			qx := dbi.Qx(ctx)
			_, _ = qx.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = $1", subID)
			_, _ = qx.Exec(ctx, "DELETE FROM openrails.payments WHERE subscription_id = $1", subID)
			_, _ = qx.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
			_, _ = qx.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", pmID)
			_, _ = qx.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
			_, _ = qx.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
			return nil
		})
	})

	f := &dunningCertaintyFixture{dbi: dbi, ctx: baseCtx, subID: subID, nmiWrites: &atomic.Int64{}}
	f.nmiRespond = func(w http.ResponseWriter) { _, _ = w.Write([]byte("response=1&transactionid=txn_ok")) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "profile" {
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><merchant><company>Certainty TEST</company><email>c@acme.test</email></merchant></nm_response>`))
			return
		}
		f.nmiWrites.Add(1)
		f.nmiRespond(w)
	}))
	t.Cleanup(srv.Close)
	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "certainty_key", WebhookSecret: "s",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL

	f.priceSvc = catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, nil)
	notifSvc := subscriptions.NewNotificationService(dbi, nil)
	paymentSvc := payments.NewPaymentService(dbi, nil)
	f.lifecycle = subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, f.priceSvc, entitlementSvc, notifSvc, paymentSvc, nil)
	// The REAL deferred-delete producer: a terminal cancellation writes an
	// nmi_delete_subscription row to the intent ledger. Its ABSENCE is what
	// these tests assert on, so it must be the production one.
	f.lifecycle.SetDeferredDeleteScheduler(
		intents.NewNMIDeleteScheduler(dbi, nil, intents.OriginSystem, "dunning terminal cancellation"))
	f.moneySvc = money.NewMoneyService(dbi, nil)
	f.subSvc = subscriptions.NewSubscriptionService(dbi, f.priceSvc, productSvc, nil, nil, nil)
	// or#865: the worker's self-assembled intent Runner parks every intent when
	// no mode is stated — these tests assert on charges that actually fire and
	// on terminal outcomes, so the mode has to be "full".
	f.worker = &DunningWorker{DB: dbi, NMIResolver: fakeDunningNMIResolver{client: client}, Config: fullModeConfig()}
	return f
}

func armDestructive(t *testing.T, f *dunningCertaintyFixture) {
	t.Helper()
	require.NoError(t, f.dbi.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		dbtest.ArmDestructiveActions(ctx, t, dbtest.TestMerchantID.UUID())
		return nil
	}))
}

func disarmDestructive(t *testing.T, f *dunningCertaintyFixture) {
	t.Helper()
	require.NoError(t, f.dbi.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		dbtest.DisarmDestructiveActions(ctx, t, f.dbi.Qx(ctx))
		return nil
	}))
}

// run executes one dunning pass over the fixture's subscription.
func (f *dunningCertaintyFixture) run(t *testing.T) dunningOutcome {
	t.Helper()
	var outcome dunningOutcome
	require.NoError(t, f.dbi.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		sub, err := f.subSvc.GetByID(ctx, f.subID)
		if err != nil {
			return err
		}
		outcome = f.worker.processSubscription(ctx, sub, f.lifecycle, f.priceSvc, f.moneySvc, false)
		return nil
	}))
	return outcome
}

type dunningRowState struct {
	status              string
	retryAttempts       *int32
	lastRetryAt         *time.Time
	nextRetryAt         *time.Time
	cancelledAt         *time.Time
	deletionScheduledAt *time.Time
	deleteIntents       int
}

func (f *dunningCertaintyFixture) state(t *testing.T) dunningRowState {
	t.Helper()
	var s dunningRowState
	require.NoError(t, f.dbi.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		qx := f.dbi.Qx(ctx)
		if err := qx.QueryRow(ctx,
			`SELECT status, retry_attempts, last_retry_at, next_retry_at, cancelled_at, deletion_scheduled_at
			   FROM openrails.subscriptions WHERE id = $1`, f.subID).
			Scan(&s.status, &s.retryAttempts, &s.lastRetryAt, &s.nextRetryAt, &s.cancelledAt, &s.deletionScheduledAt); err != nil {
			return err
		}
		return qx.QueryRow(ctx,
			`SELECT count(*) FROM openrails.rail_intents
			  WHERE subscription_id = $1 AND intent_type = 'nmi_delete_subscription'`, f.subID).Scan(&s.deleteIntents)
	}))
	return s
}

// assertParkedNotDestroyed is the doctrine in one place: the row is in
// verification limbo, NOT cancelled, and no irreversible provider delete was
// ever queued for it.
func assertParkedNotDestroyed(t *testing.T, s dunningRowState) {
	t.Helper()
	assert.Equal(t, string(models.StatusUnknown), s.status,
		"the row must PARK for provider verification, not terminate")
	assert.Nil(t, s.cancelledAt, "cancellation is a last resort; nothing here justified it")
	assert.Nil(t, s.deletionScheduledAt, "no deletion marker may be written")
	assert.Zero(t, s.deleteIntents,
		"an NMI vault delete is IRREVERSIBLE — the stored card is destroyed and only the cardholder can restore it")
	assert.Nil(t, s.nextRetryAt, "a parked row leaves the dunning queue; the provider probe owns it now")
}

// #840: our own payment_methods row is missing its vault refs. That is the
// absence of OUR data — a vault-sync bug, a failed write, a mis-populated ref —
// not a provider decline, and there is no forensic payments row to justify
// anything. On a daily cycle DunningMaxFailures is 1, so before the fix the
// FIRST observation was terminal: cancel + revoke + queued NMI vault delete.
func TestDunning_MissingLocalPaymentMethodParksAndNeverDeletesTheVault(t *testing.T) {
	// Daily cycle, one hour past the period end: comfortably IN window, so the
	// only thing under test is the missing local row.
	f := newDunningCertaintyFixture(t, 24, time.Hour, false)
	// Destructive actions fully ARMED: the refusal must come from the absent
	// evidence, not from the kill switch.
	armDestructive(t, f)
	t.Cleanup(func() { disarmDestructive(t, f) })

	before := f.state(t)
	require.Equal(t, string(models.StatusPastDue), before.status)

	outcome := f.run(t)
	assert.Equal(t, dunningOutcomeFailed, outcome)
	assert.Zero(t, f.nmiWrites.Load(), "nothing chargeable exists; no provider mutation may be sent")

	s := f.state(t)
	assertParkedNotDestroyed(t, s)

	// The absence of our own data must never be counted as a dunning attempt:
	// no retry ordinal burned, no claim stamped on the forensic last_retry_at.
	assert.Nil(t, s.retryAttempts, "an attempt we never made cannot count toward retry_attempts")
	assert.Nil(t, s.lastRetryAt, "the claim must not run: last_retry_at is dunning forensic evidence")

	var paymentRows int
	require.NoError(t, f.dbi.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		return f.dbi.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.payments WHERE subscription_id = $1`, f.subID).Scan(&paymentRows)
	}))
	assert.Zero(t, paymentRows, "no charge was attempted, so no payment row is invented either")
}

// #839: a short-cycle subscription past its period end, with NO decline
// anywhere. Window(24h) derived ZERO before the fix, so `now > periodEnd + 0`
// was true the instant the row lapsed — the sub was terminally cancelled and
// its vault queued for deletion on the first dunning touch, having never been
// charged once. A clock reading is not a death certificate: NMI rebills
// forever, so a lapsed date is the NORMAL state of a dunning customer.
func TestDunning_ShortCycleStaleSubscriptionParksAndIsNeverTerminated(t *testing.T) {
	// Daily cycle, five days past the period end — genuinely stale, well past
	// any window. The charge must be SKIPPED (never surprise-charge a stale
	// rebill) and the row parked, not destroyed.
	f := newDunningCertaintyFixture(t, 24, 5*24*time.Hour, true)
	armDestructive(t, f)
	t.Cleanup(func() { disarmDestructive(t, f) })

	outcome := f.run(t)
	assert.Equal(t, dunningOutcomeWindowExpired, outcome)
	assert.Zero(t, f.nmiWrites.Load(), "a months-stale rebill is never surprise-charged by a catch-up run")

	assertParkedNotDestroyed(t, f.state(t))
}

// #839, the other half: a short-cycle subscription that has only JUST lapsed
// must still get its charge. Before the fix the zero-length window swallowed it
// on the first touch; a zero window must mean "never auto-terminate", not
// "always terminate", and it must not mean "never bill" either.
func TestDunning_ShortCycleJustLapsedIsStillCharged(t *testing.T) {
	f := newDunningCertaintyFixture(t, 24, time.Hour, true)
	armDestructive(t, f)
	t.Cleanup(func() { disarmDestructive(t, f) })

	outcome := f.run(t)
	assert.Equal(t, dunningOutcomeSucceeded, outcome, "a daily sub one hour past due is IN window and gets its rebill")
	assert.Positive(t, f.nmiWrites.Load(), "the charge the subscription is queued for actually fires")

	s := f.state(t)
	assert.Nil(t, s.cancelledAt)
	assert.Zero(t, s.deleteIntents)
}

// #836: the operator kill switch must halt TERMINAL COLLECTION OUTCOMES, not
// just the converge sweep. or#870 bucket 3 is the one path in dunning that still
// legitimately terminates — with the switch off it must park instead, and no
// provider destruction may be queued.
func TestDunning_KillSwitchHaltsTerminalCollectionOutcomes(t *testing.T) {
	hardDecline := func(w http.ResponseWriter) {
		// 261 Stop All Recurring Payments — the issuer has withdrawn the
		// recurring mandate. or#870 bucket 3: the only class of decline that
		// terminates, and only when the operator has armed destructive actions.
		// (201 Do Not Honor used to sit here; it is bucket 2 now — the customer
		// can fix that one, so it never terminates whatever the switch says.)
		_, _ = w.Write([]byte("response=2&response_code=261&responsetext=Declined - Stop All Recurring Payments"))
	}

	// --- switch OFF (the shipped default): the decline lands, nothing dies ---
	off := newDunningCertaintyFixture(t, 24, time.Hour, true)
	off.nmiRespond = hardDecline
	disarmDestructive(t, off)

	assert.Equal(t, dunningOutcomeFailed, off.run(t))
	assert.Positive(t, off.nmiWrites.Load(), "the kill switch stops destruction, not the charge attempt itself")
	assertParkedNotDestroyed(t, off.state(t))

	// --- operator arms it: the SAME decline on the SAME shape terminates -----
	on := newDunningCertaintyFixture(t, 24, time.Hour, true)
	on.nmiRespond = hardDecline
	armDestructive(t, on)
	t.Cleanup(func() { disarmDestructive(t, on) })

	assert.Equal(t, dunningOutcomeFailed, on.run(t))
	s := on.state(t)
	assert.Equal(t, string(models.StatusCancelled), s.status,
		"a real non-retryable-class gateway decline IS evidence; armed, it terminates")
	assert.NotNil(t, s.cancelledAt)
	assert.Equal(t, 1, s.deleteIntents,
		"and only then is the remote NMI schedule stopped through the deferred-delete mechanism")
}
