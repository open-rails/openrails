//go:build integration

package embedded

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #737 fixture matrix: a declared legacy book lands through the decider at the
// AsOf horizon — active-with-runway adopts, mid-dunning enters past_due with
// grace, explicit cancels keep faithful cancel_type/dates, evidence-starved
// rows park as unknown, charges land in micros idempotently, and a re-import
// is a no-op.
func TestImportBilling_DeclaredBookClassifiesAtAsOf(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	appDB, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	sfx := uuid.NewString()[:8]
	prod, price := uuid.New(), uuid.New()
	asOf := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	cust := func() uuid.UUID { return uuid.New() }
	cRunway, cDunning, cLapsed, cUser, cChargeback, cParked := cust(), cust(), cust(), cust(), cust(), cust()
	allCust := []uuid.UUID{cRunway, cDunning, cLapsed, cUser, cChargeback, cParked}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		_, e := appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{"premium":null}'::jsonb,$3)`,
			prod, "imp-"+sfx, merchantID)
		require.NoError(t, e)
		_, e = appDB.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id) VALUES ($1,$2,23000000,'usd',720,true,$3)`,
			price, prod, merchantID)
		require.NoError(t, e)
		return nil
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			for _, c := range allCust {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.entitlements WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.grants WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payments WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.subscriptions WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payment_methods WHERE merchant_id=$1 AND customer_id=$2`, merchantID, c)
			}
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
			return nil
		})
	})

	paidRunway := asOf.Add(20 * day)
	paidDunning := asOf.Add(-5 * day)
	lastRetryAt := asOf.Add(-1 * day)
	lapsedAt := asOf.Add(-2 * 365 * day)
	userCancelAt := asOf.Add(-10 * day)
	userPaidThrough := asOf.Add(5 * day)
	chargebackAt := asOf.Add(-30 * day)
	parkedPaid := asOf.Add(-3 * 365 * day)

	book := DeclaredBilling{
		AsOf: asOf,
		Customers: []DeclaredCustomer{
			{Customer: cRunway}, {Customer: cDunning}, {Customer: cLapsed},
			{Customer: cUser}, {Customer: cChargeback}, {Customer: cParked},
		},
		PaymentMethods: []DeclaredPaymentMethod{{
			Customer: cDunning, Rail: "nmi", RailCustomerRef: "vault-" + sfx, RailMethodRef: "",
			InitialTransactionID: "itx-" + sfx, LastFour: "4242",
		}},
		Subscriptions: []DeclaredSubscription{
			{
				SourceID: "runway", Customer: cRunway, Price: price, Rail: "nmi",
				RailSubscriptionID: "sub-runway-" + sfx, StartedAt: asOf.Add(-100 * day), PaidThrough: &paidRunway,
			},
			{
				SourceID: "dunning", Customer: cDunning, Price: price, Rail: "nmi",
				RailSubscriptionID: "sub-dunning-" + sfx, StartedAt: asOf.Add(-200 * day), PaidThrough: &paidDunning,
				Dunning:       &DunningEvidence{Retries: 2, LastRetryAt: &lastRetryAt, ScheduleLive: true},
				PaymentMethod: &PaymentMethodRef{Rail: "nmi", RailCustomerRef: "vault-" + sfx, RailMethodRef: ""},
				Evidence:      json.RawMessage(`{"legacy_source":"subscriptions","legacy_id":42}`),
			},
			{
				SourceID: "lapsed", Customer: cLapsed, Price: price, Rail: "nmi",
				RailSubscriptionID: "sub-lapsed-" + sfx, StartedAt: asOf.Add(-3 * 365 * day), PaidThrough: &lapsedAt,
				Cancel: CancelEvidence{Kind: "provider_terminated", At: lapsedAt},
			},
			{
				SourceID: "usercancel", Customer: cUser, Price: price, Rail: "ccbill",
				RailSubscriptionID: "sub-user-" + sfx, StartedAt: asOf.Add(-90 * day), PaidThrough: &userPaidThrough,
				Cancel: CancelEvidence{Kind: "user_cancelled", At: userCancelAt},
			},
			{
				SourceID: "chargeback", Customer: cChargeback, Price: price, Rail: "nmi",
				RailSubscriptionID: "sub-cb-" + sfx, StartedAt: asOf.Add(-120 * day),
				Cancel: CancelEvidence{Kind: "chargeback", At: chargebackAt},
			},
			{
				SourceID: "parked", Customer: cParked, Price: price, Rail: "nmi",
				RailSubscriptionID: "sub-parked-" + sfx, StartedAt: asOf.Add(-4 * 365 * day), PaidThrough: &parkedPaid,
			},
		},
		Transactions: []DeclaredTransaction{
			// Runway's opening charge: cents → micros pin (2300 → 23_000_000).
			{RailSubscriptionID: "sub-runway-" + sfx, TransactionID: "tx-open-" + sfx, Success: true, AmountCents: 2300, Currency: "usd", OccurredAt: asOf.Add(-10 * day)},
			// Dunning's failed renewal, within the window.
			{RailSubscriptionID: "sub-dunning-" + sfx, TransactionID: "tx-decl-" + sfx, Type: "decline", Success: false, AmountCents: 2300, Currency: "usd", OccurredAt: asOf.Add(-4 * day)},
			// Chargeback's reversal.
			{RailSubscriptionID: "sub-cb-" + sfx, TransactionID: "tx-cb-" + sfx, Type: "chargeback", Success: false, AmountCents: 2300, Currency: "usd", OccurredAt: chargebackAt},
		},
	}

	res, err := ImportBilling(context.Background(), BillingImportOptions{
		PGXPool: pool, MerchantSlug: dbtest.TestMerchantSlug, Book: book,
	})
	require.NoError(t, err)
	require.Empty(t, res.Blocked, "no blocks expected: %v", res.Reasons)
	require.ElementsMatch(t, []string{"runway", "dunning", "lapsed", "usercancel", "chargeback", "parked"}, res.Imported)

	type subRow struct {
		status, cancelType             string
		cancelledAt, endedAt, graceEnd *time.Time
		periodStart, periodEnd         *time.Time
		pmLinked                       bool
	}
	load := func(ctx context.Context, railSubID string) subRow {
		var r subRow
		var ct *string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT status::text, COALESCE(cancel_type::text,''), cancelled_at, ended_at, grace_ends_at,
			        current_period_starts_at, current_period_ends_at, payment_method_id IS NOT NULL
			 FROM openrails.subscriptions WHERE rail_subscription_id=$1`, railSubID).
			Scan(&r.status, &ct, &r.cancelledAt, &r.endedAt, &r.graceEnd, &r.periodStart, &r.periodEnd, &r.pmLinked))
		if ct != nil {
			r.cancelType = *ct
		}
		return r
	}

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		// 1) Active with runway: adopted at the declared paid-through.
		r := load(ctx, "sub-runway-"+sfx)
		require.Equal(t, "active", r.status)
		require.NotNil(t, r.periodEnd)
		require.True(t, r.periodEnd.Equal(paidRunway), "period end = declared paid-through")

		// 2) Mid-dunning: past_due with grace = period end + 48h.
		r = load(ctx, "sub-dunning-"+sfx)
		require.Equal(t, "past_due", r.status)
		require.NotNil(t, r.graceEnd)
		require.True(t, r.graceEnd.Equal(paidDunning.Add(48*time.Hour)), "grace = missed period end + PeriodGrace")
		require.True(t, r.pmLinked, "dunning sub linked to its declared vault")

		// 2b) Seed-time forensics: Evidence → gateway_response, Dunning history
		// → retry_attempts/last_retry_at.
		var legacySource string
		var retries int
		var lastRetry *time.Time
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT COALESCE(gateway_response->>'legacy_source',''), retry_attempts, last_retry_at
			 FROM openrails.subscriptions WHERE rail_subscription_id=$1`, "sub-dunning-"+sfx).
			Scan(&legacySource, &retries, &lastRetry))
		require.Equal(t, "subscriptions", legacySource, "declared Evidence lands on gateway_response")
		require.Equal(t, 2, retries)
		require.NotNil(t, lastRetry)
		require.True(t, lastRetry.Equal(lastRetryAt))

		// 3) Years-lapsed provider_terminated: faithful dates, expired type.
		r = load(ctx, "sub-lapsed-"+sfx)
		require.Equal(t, "cancelled", r.status)
		require.Equal(t, "expired", r.cancelType)
		require.NotNil(t, r.cancelledAt)
		require.True(t, r.cancelledAt.Equal(lapsedAt), "cancelled_at = declared evidence date, not import wall-clock")

		// 4) User cancel with runway: cancel_type user, access ends at paid-through.
		r = load(ctx, "sub-user-"+sfx)
		require.Equal(t, "cancelled", r.status)
		require.Equal(t, "user", r.cancelType)
		require.NotNil(t, r.endedAt)
		require.True(t, r.endedAt.Equal(userPaidThrough), "cancel-with-runway keeps the paid-through end")

		// 5) Chargeback: faithful type.
		r = load(ctx, "sub-cb-"+sfx)
		require.Equal(t, "cancelled", r.status)
		require.Equal(t, "chargeback", r.cancelType)

		// 6) Evidence-starved: parked unknown (cancellation-last-resort).
		r = load(ctx, "sub-parked-"+sfx)
		require.Equal(t, "unknown", r.status)

		// 7) Charges landed in MICROS, linked to the sub (cents → micros pin).
		var amt int64
		var payStatus string
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT amount, status FROM openrails.payments WHERE transaction_id=$1`, "tx-open-"+sfx).Scan(&amt, &payStatus))
		require.Equal(t, int64(23_000_000), amt, "2300 cents → 23,000,000 micros")
		require.Equal(t, "completed", payStatus)
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT amount, status FROM openrails.payments WHERE transaction_id=$1`, "tx-decl-"+sfx).Scan(&amt, &payStatus))
		require.Equal(t, "failed", payStatus, "declines land as the true attempt history")
		return nil
	}))

	// 8) Re-import: pure no-op — everything already present, no dup payments,
	// no lifecycle regression.
	res2, err := ImportBilling(context.Background(), BillingImportOptions{
		PGXPool: pool, MerchantSlug: dbtest.TestMerchantSlug, Book: book,
	})
	require.NoError(t, err)
	require.Empty(t, res2.Imported)
	require.Empty(t, res2.Blocked, "re-import blocks: %v", res2.Reasons)
	require.ElementsMatch(t, []string{"runway", "dunning", "lapsed", "usercancel", "chargeback", "parked"}, res2.Skipped)

	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.payments WHERE transaction_id LIKE '%'||$1`, sfx).Scan(&n))
		require.Equal(t, 3, n, "payments idempotent by (rail, transaction_id)")
		r := load(ctx, "sub-user-"+sfx)
		require.Equal(t, "user", r.cancelType, "re-import never regresses settled history")
		r = load(ctx, "sub-runway-"+sfx)
		require.Equal(t, "active", r.status)
		return nil
	}))

	// 9) Cross-rail lifecycle twin: a SECOND live sub for the same customer +
	// product (different rail key) cannot adopt into the occupied lifecycle
	// slot — it blocks loudly, stays parked unknown, and never fails the batch.
	twin := DeclaredBilling{
		AsOf: asOf,
		Subscriptions: []DeclaredSubscription{{
			SourceID: "twin", Customer: cRunway, Price: price, Rail: "ccbill",
			RailSubscriptionID: "sub-twin-" + sfx, StartedAt: asOf.Add(-50 * day), PaidThrough: &paidRunway,
		}},
	}
	res3, err := ImportBilling(context.Background(), BillingImportOptions{
		PGXPool: pool, MerchantSlug: dbtest.TestMerchantSlug, Book: twin,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"twin"}, res3.Blocked)
	require.Contains(t, res3.Reasons["twin"], "lifecycle conflict")
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		r := load(ctx, "sub-twin-"+sfx)
		require.Equal(t, "unknown", r.status, "blocked twin stays parked, never stomps the live owner")
		r = load(ctx, "sub-runway-"+sfx)
		require.Equal(t, "active", r.status)
		return nil
	}))
}
