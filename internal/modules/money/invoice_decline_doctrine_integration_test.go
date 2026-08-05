//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// or#828: the invoice/arrears consumer of the or#870 decline doctrine.
//
// The subscription consumer already classified with collection.ClassifyDecline;
// the invoice consumer called the older two-way FailureAction, which had no
// bucket-3 concept and returned a bare Action{} — no next attempt AND not
// terminal — for every code it called non-retryable. These tests pin the three
// buckets on the INVOICE path, through the production entry point
// (ChargeOutstanding) under the RLS-enforcing harness.
//
// The decision logic is identical to the subscription path; the CONSEQUENCES
// differ because the products differ. An invoice is not a subscription: no
// bucket cancels one, and no bucket revokes access. What unpaid arrears does to
// SERVICE is the separate, time-based, opt-in delinquency axis (or#878).

// codeDeclineCharger declines every charge with one verbatim rail code, the way
// a real gateway does.
type codeDeclineCharger struct {
	rail    string
	code    string
	message string
	charges []money.ChargeRequest
}

func (c *codeDeclineCharger) ChargeSavedMethod(_ context.Context, req money.ChargeRequest) (money.ChargeResult, error) {
	c.charges = append(c.charges, req)
	code, message := c.code, c.message
	if message == "" {
		message = "declined"
	}
	return money.ChargeResult{
		Rail: c.rail, Declined: true, FailureCode: &code, FailureMessage: &message,
	}, nil
}

// declineDoctrineEnv seeds an arrears payer with a saved NMI method and an
// ACTIVE subscription on the same payer, then finalizes ONE receivable whose
// statement period is exactly `cycle` long — the invoice's real billing cycle,
// which is what the retry schedule now derives from.
//
// The subscription is the control: no invoice decline bucket may touch it.
func declineDoctrineEnv(t *testing.T, cycle time.Duration, clock *clockwork.FakeClock) (
	*money.MoneyService, *db.DB, *pgxpool.Pool, identity.CustomerID, string, uuid.UUID, uuid.UUID, context.Context,
) {
	t.Helper()
	_, dbi, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	cleanupInvoices(t, pool, ctx, payer)
	svc := money.NewMoneyService(dbi, clock)

	method := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &method,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "doctrine-"+uuid.NewString(), 5_000_000)
	require.NoError(t, err)

	subID := seedActiveSubscription(t, dbi, pool, ctx, payer)

	// The accrual lands AT clock-now and the finalize window is [from, to), so
	// the frozen clock must advance past it.
	clock.Advance(time.Minute)
	to := clock.Now()
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, to.Add(-cycle), to)
	require.NoError(t, err)
	require.Equal(t, "open", invoice.Status, "a finalized arrears receivable starts open")

	return svc, dbi, pool, payer, currency, invoice.ID, subID, ctx
}

// seedActiveSubscription puts a live subscription on the payer so "did the
// invoice path cancel a subscription?" is answered by reading the real row.
func seedActiveSubscription(t *testing.T, dbi *db.DB, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID) uuid.UUID {
	t.Helper()
	q := dbi.Gen(ctx)
	now := time.Now().UTC()
	periodEnd := now.AddDate(0, 1, 0)
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	cycleHours := int32(30 * 24)

	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, MerchantID: dbtest.TestMerchantID.UUID(),
		Key: "doctrine_control_" + uuid.NewString(), DisplayName: "Doctrine Control",
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, MerchantID: dbtest.TestMerchantID.UUID(), ProductID: productID,
		Amount: 100, Currency: "USD", AccessDurationHours: &cycleHours, AutoRenew: true,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailNMI))
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: subID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: payer.UUID(),
		ProductID: productID, PriceID: &priceID,
		Status: string(models.StatusActive), Rail: string(models.RailNMI), PspID: pspID,
		RailSubscriptionID:    "sub_doctrine_" + subID.String(),
		CurrentPeriodStartsAt: &now, CurrentPeriodEndsAt: &periodEnd, StartedAt: now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	return subID
}

func requireSubscriptionUntouched(t *testing.T, pool *pgxpool.Pool, ctx context.Context, subID uuid.UUID) {
	t.Helper()
	var status string
	var cancelledAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, cancelled_at FROM openrails.subscriptions WHERE id = $1`, subID).
		Scan(&status, &cancelledAt))
	require.Equal(t, string(models.StatusActive), status,
		"an invoice decline must never cancel a subscription — an invoice is not a subscription")
	require.Nil(t, cancelledAt)
}

func requireStoredMethodsIntact(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID, want int) {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.payment_methods WHERE customer_id = $1`, payer.UUID()).Scan(&n))
	require.Equal(t, want, n, "OpenRails NEVER deletes a stored payment method — only the end user does")
}

func notificationEventTypes(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT event_type FROM openrails.notification_queue WHERE customer_id = $1 ORDER BY created_at, id`,
		payer.UUID())
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		require.NoError(t, rows.Scan(&e))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

func notificationData(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID, eventType string) map[string]any {
	t.Helper()
	var data map[string]any
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT data FROM openrails.notification_queue
		 WHERE customer_id = $1 AND event_type = $2 ORDER BY created_at DESC, id DESC LIMIT 1`,
		payer.UUID(), eventType).Scan(&data))
	return data
}

func cleanupNotifications(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID) {
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", payer.UUID())
	})
}

// TestInvoiceDecline_Bucket2_StopsChargingKeepsInvoiceOpenAndNotifies is the
// bucket the old code got most wrong. `223` (expired card) hit
// RetryableFailureReason, came back false, and produced a bare Action{}: no
// next attempt, NOT terminal, no reason recorded, no notice sent. The invoice
// parked in a state nothing resolved.
//
// The doctrine's answer: stop charging, LEAVE THE INVOICE OPEN and owed, keep
// access, tell the customer to fix their card — and make fixing it actually
// resume collection.
func TestInvoiceDecline_Bucket2_StopsChargingKeepsInvoiceOpenAndNotifies(t *testing.T) {
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))
	svc, _, pool, payer, currency, invoiceID, subID, ctx := declineDoctrineEnv(t, 30*24*time.Hour, clock)
	cleanupNotifications(t, pool, ctx, payer)

	charger := &codeDeclineCharger{rail: "nmi", code: "223", message: "expired card"}
	n, err := svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Len(t, charger.charges, 1)

	stopped, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
	require.NoError(t, err)
	require.Equal(t, "open", stopped.Status,
		"bucket 2 stops charging; it does not age the invoice — that is the delinquency axis (or#878)")
	require.Nil(t, stopped.UncollectibleAt, "bucket 2 is emphatically NOT terminal")
	require.Nil(t, stopped.NextCollectionAttemptAt, "charging STOPS — no attempt is scheduled")
	require.Equal(t, int32(1), stopped.CollectionFailureCount)
	require.Equal(t, int64(5_000_000), stopped.AmountDue, "the debt stands in full")
	require.NotNil(t, stopped.LastCollectionFailureCode)
	require.Equal(t, "223", *stopped.LastCollectionFailureCode, "the rail's code is recorded verbatim")

	// Charging really stopped: the scheduled sweep does not pick it up again,
	// however many times it runs.
	for i := 0; i < 3; i++ {
		clock.Advance(48 * time.Hour)
		n, err = svc.ChargeOutstanding(ctx, charger, 0)
		require.NoError(t, err)
		require.Zero(t, n)
	}
	require.Len(t, charger.charges, 1, "no further attempt against an instrument that cannot succeed")

	// Access and the instrument are untouched.
	requireSubscriptionUntouched(t, pool, ctx, subID)
	requireStoredMethodsIntact(t, pool, ctx, payer, 1)

	// The customer was told what to do.
	require.Equal(t, []string{string(models.NotificationPaymentMethodUpdateRequired)},
		notificationEventTypes(t, pool, ctx, payer))
	data := notificationData(t, pool, ctx, payer, string(models.NotificationPaymentMethodUpdateRequired))
	require.Equal(t, invoiceID.String(), data["invoice_id"])
	require.Equal(t, "223", data["failure_code"])
	require.Equal(t, "fix_payment_method", data["decline_outcome"])

	// ...and doing it resumes collection, which is what makes this a stop
	// rather than the dead state or#828 was filed on.
	fixed := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, currency, fixed))

	good := &fakeCharger{}
	n, err = svc.ChargeOutstanding(ctx, good, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n, "a fixed payment method resumes the stopped invoice")
	require.Len(t, good.charges, 1)
	require.Equal(t, fixed, good.charges[0].PaymentMethodID)

	paid, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
}

// TestInvoiceDecline_Bucket3_MarksUncollectibleWithoutCancellingAnything: the
// issuer withdrew the recurring mandate. Stop, mark the invoice
// non-recoverable, notify. The old code had no bucket-3 concept at all — its
// only road to terminal was schedule exhaustion, so `261` was treated exactly
// like insufficient funds that ran out of retries.
func TestInvoiceDecline_Bucket3_MarksUncollectibleWithoutCancellingAnything(t *testing.T) {
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))
	svc, _, pool, payer, _, invoiceID, subID, ctx := declineDoctrineEnv(t, 30*24*time.Hour, clock)
	cleanupNotifications(t, pool, ctx, payer)

	charger := &codeDeclineCharger{rail: "nmi", code: "261", message: "stop all recurring payments"}
	n, err := svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Len(t, charger.charges, 1)

	dead, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
	require.NoError(t, err)
	require.Equal(t, "uncollectible", dead.Status, "bucket 3 is terminal on the FIRST look")
	require.NotNil(t, dead.UncollectibleAt)
	require.Nil(t, dead.NextCollectionAttemptAt)
	require.Equal(t, int32(1), dead.CollectionFailureCount,
		"terminal without burning the schedule — the mandate is gone, retrying cannot help")

	clock.Advance(30 * 24 * time.Hour)
	n, err = svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Len(t, charger.charges, 1)

	// An invoice is not a subscription: going uncollectible cancels nothing and
	// destroys nothing.
	requireSubscriptionUntouched(t, pool, ctx, subID)
	requireStoredMethodsIntact(t, pool, ctx, payer, 1)

	require.Equal(t, []string{string(models.NotificationInvoiceCollectionStopped)},
		notificationEventTypes(t, pool, ctx, payer))
	data := notificationData(t, pool, ctx, payer, string(models.NotificationInvoiceCollectionStopped))
	require.Equal(t, "non_recoverable", data["reason"])
	require.Equal(t, "261", data["failure_code"])
}

// TestInvoiceDecline_Bucket1_RetriesOnTheInvoicesOwnCycle: the hardcode.
// Every invoice was dunned on collection.MonthlyCycleHours regardless of what
// it actually billed, so a WEEKLY statement retried on the monthly offsets
// (+2d) instead of its own (+1d).
func TestInvoiceDecline_Bucket1_RetriesOnTheInvoicesOwnCycle(t *testing.T) {
	t.Run("weekly statement retries on the weekly offsets", func(t *testing.T) {
		clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))
		svc, _, pool, payer, _, invoiceID, subID, ctx := declineDoctrineEnv(t, 7*24*time.Hour, clock)
		cleanupNotifications(t, pool, ctx, payer)

		charger := &codeDeclineCharger{rail: "nmi", code: "202", message: "insufficient funds"}
		_, err := svc.ChargeOutstanding(ctx, charger, 0)
		require.NoError(t, err)

		row, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
		require.NoError(t, err)
		require.NotNil(t, row.CollectionFailedAt)
		require.NotNil(t, row.NextCollectionAttemptAt)
		require.True(t, row.NextCollectionAttemptAt.Equal(row.CollectionFailedAt.Add(24*time.Hour)),
			"a 7-day statement retries at +1d (its own cycle), not at +2d (a hardcoded month); got %s",
			row.NextCollectionAttemptAt.Sub(*row.CollectionFailedAt))
		require.Equal(t, "past_due", row.Status, "bucket 1 is still dunning, which IS the past-due signal")

		require.Equal(t, []string{string(models.NotificationPaymentMethodFailed)},
			notificationEventTypes(t, pool, ctx, payer))

		// Its schedule is the weekly one end to end: 2 retries, then terminal.
		for i := 0; i < 2; i++ {
			clock.Advance(row.NextCollectionAttemptAt.Sub(clock.Now()) + time.Minute)
			_, err = svc.ChargeOutstanding(ctx, charger, 0)
			require.NoError(t, err)
			row, err = svc.GetInvoiceByID(ctx, payer, invoiceID)
			require.NoError(t, err)
		}
		require.Equal(t, "uncollectible", row.Status, "the weekly schedule tolerates 3 failures, not 5")
		require.Equal(t, int32(3), row.CollectionFailureCount)
		require.Len(t, charger.charges, 3)

		// Exhaustion says "we gave up", not "the mandate is gone".
		data := notificationData(t, pool, ctx, payer, string(models.NotificationInvoiceCollectionStopped))
		require.Equal(t, "schedule_exhausted", data["reason"])
		requireSubscriptionUntouched(t, pool, ctx, subID)
		requireStoredMethodsIntact(t, pool, ctx, payer, 1)
	})

	t.Run("monthly statement retries on the monthly offsets", func(t *testing.T) {
		clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))
		svc, _, pool, payer, _, invoiceID, _, ctx := declineDoctrineEnv(t, 30*24*time.Hour, clock)
		cleanupNotifications(t, pool, ctx, payer)

		charger := &codeDeclineCharger{rail: "nmi", code: "202", message: "insufficient funds"}
		_, err := svc.ChargeOutstanding(ctx, charger, 0)
		require.NoError(t, err)

		row, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
		require.NoError(t, err)
		require.NotNil(t, row.NextCollectionAttemptAt)
		require.True(t, row.NextCollectionAttemptAt.Equal(row.CollectionFailedAt.Add(48*time.Hour)),
			"a monthly statement retries at +2d")
	})
}

// TestInvoiceDecline_UnknownCodeRetries: missing evidence must never cost a
// customer anything. An unrecognized code is bucket 1 by construction — the
// zero value of the outcome — so it schedules a retry instead of parking.
// Before the doctrine this landed in the bare Action{}: no retry, no terminal,
// nothing to resolve it.
func TestInvoiceDecline_UnknownCodeRetries(t *testing.T) {
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))
	svc, _, pool, payer, _, invoiceID, subID, ctx := declineDoctrineEnv(t, 30*24*time.Hour, clock)
	cleanupNotifications(t, pool, ctx, payer)

	// A code no published NMI table names, in the wire form the charge path
	// actually records (#733 no-fabrication).
	charger := &codeDeclineCharger{rail: "nmi", code: "nmi_response_997", message: "who knows"}
	_, err := svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)

	row, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
	require.NoError(t, err)
	require.NotNil(t, row.NextCollectionAttemptAt, "an unreadable code retries; it never parks and never terminates")
	require.Nil(t, row.UncollectibleAt)
	requireSubscriptionUntouched(t, pool, ctx, subID)

	// And it really does retry: advancing to the scheduled moment charges again.
	clock.Advance(row.NextCollectionAttemptAt.Sub(clock.Now()) + time.Minute)
	_, err = svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Len(t, charger.charges, 2)
}

// TestInvoiceDecline_CCBillEveryCodeRetries: CCBill publishes no enumerated
// decline vocabulary, so nothing it returns can justify stopping. Even the
// STRING that means "stop all recurring payments" on NMI must retry here —
// refusing to act on a code we cannot read is the correct answer, not a gap.
func TestInvoiceDecline_CCBillEveryCodeRetries(t *testing.T) {
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))
	svc, _, pool, payer, _, invoiceID, subID, ctx := declineDoctrineEnv(t, 30*24*time.Hour, clock)
	cleanupNotifications(t, pool, ctx, payer)

	charger := &codeDeclineCharger{rail: "ccbill", code: "declined_stop_all_recurring_payments"}
	_, err := svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)

	row, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
	require.NoError(t, err)
	require.NotNil(t, row.NextCollectionAttemptAt,
		"a decline string OpenRails cannot read may not stop collection on any rail")
	require.Nil(t, row.UncollectibleAt)
	requireSubscriptionUntouched(t, pool, ctx, subID)
}
