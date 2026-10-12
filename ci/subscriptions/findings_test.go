//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

const duplicateCharge = "consistency.duplicate.provider_charge"

var cadences = []int{1, 24, 7 * 24, monthHours, 90 * 24, 365 * 24}

// Duplicate-charge findings follow each subscription's own periods at every
// cadence: two captures of one period are one CRITICAL finding on the later
// one; consecutive periods, a declined-then-retried charge and a price change
// are not duplicates; a finding the scan no longer reports closes.
func TestDuplicateChargeFindings(t *testing.T) {
	t.Parallel()
	for _, hours := range cadences {
		t.Run(fmt.Sprintf("%dh", hours), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.waive("evidenced", "the scenario writes duplicate payment rows directly to exercise the detector")
			w.waive("once", "the scenario writes duplicate payment rows directly to exercise the detector")
			clean := enrollEvery(t, w, "nmi", embedded, hours)
			dup := enrollEvery(t, w, "nmi", embedded, hours)
			retried := enrollEvery(t, w, "stripe", embedded, hours)
			stale := w.seedFinding(duplicateCharge, "provider_charge:"+clean.c.id+":"+uuid.NewString()+":"+w.clock.Now().Format("2006-01"))
			retried.refreshBeforePeriodEnd()
			retried.setDecline(visa.Last4, "insufficient_funds", "202")
			for i := range 3 {
				end := clean.periodEnd()
				clean.toFreshPeriodEnd()
				w.runRenewals()
				require.True(t, clean.periodEnd().After(end))
				if i == 0 {
					require.Equal(t, billing.SubscriptionPastDue, w.subscription(embedded, retried.sub).Status)
					retried.replaceCard(mastercard)
					w.runRenewals()
					require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, retried.sub).Status, "the retry renews")
				}
			}
			require.Len(t, completed(w.payments(embedded, clean.c.id)), 4)
			w.converge()
			require.Empty(t, w.openFindings(duplicateCharge), "consecutive periods and a declined-then-retried period are not duplicates")
			require.NotContains(t, w.openFindings(duplicateCharge), stale, "the calendar-month false positive closes")

			// A separate adjustment bill on the same subscription is its own
			// obligation. It stays: its order holds that payment as settlement.
			w.chargeAgain(clean.c.id, time.Minute, true)
			w.converge()
			require.Empty(t, w.openFindings(duplicateCharge), "a separate billed adjustment is not a duplicate")

			// A second capture of one period, a day later (across a month
			// boundary for long cadences): one CRITICAL finding naming the
			// subscription, targeting the later charge.
			later := w.chargeAgain(dup.c.id, 24*time.Hour, false)
			w.converge()
			findings := w.findings(duplicateCharge)
			require.Len(t, findings, 1)
			require.Equal(t, "critical", findings[0].severity)
			require.Contains(t, findings[0].subject, strings.TrimPrefix(dup.sub.String(), "sub_"))
			require.Contains(t, findings[0].action, "pay_"+later)
			require.Contains(t, findings[0].action, "refund only the verified excess")
			require.Regexp(t, `; net [0-9]+\.[0-9]{2} USD; payments `, findings[0].action, "the total reads in its currency")
			w.dropLatestCharge(dup.c.id)
			w.converge()
			require.Empty(t, w.openFindings(duplicateCharge), "the finding closes once the duplicate is gone")

			// Removing optional period metadata cannot hide the order identity.
			w.forgetPaidPeriods(clean.c.id)
			w.converge()
			require.Empty(t, w.openFindings(duplicateCharge))
			w.chargeAgain(clean.c.id, time.Minute, false)
			w.converge()
			require.Len(t, w.openFindings(duplicateCharge), 1, "excess captures of one order do not depend on metadata")
		})
	}
}

type findingRow struct{ subject, severity, action string }

func (w *world) findings(findingType string) []findingRow {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT subject_key, severity, coalesce(recommended_action, '') FROM billing.reconciliation_findings
		WHERE finding_type = $1 AND status IN ('reconcile_required', 'requires_review')`), findingType)
	require.NoError(w.t, err)
	defer rows.Close()
	var out []findingRow
	for rows.Next() {
		var f findingRow
		require.NoError(w.t, rows.Scan(&f.subject, &f.severity, &f.action))
		out = append(out, f)
	}
	return out
}

func (w *world) q(sql string) string {
	return inSchema(w.schema, sql)
}

// seedFinding stands in for an open finding a previous detector raised.
func (w *world) seedFinding(findingType, subject string) string {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`INSERT INTO billing.reconciliation_findings (merchant_id, finding_type, subject_key, severity, status)
		SELECT id, $2, $3, 'critical', 'requires_review' FROM billing.merchants WHERE slug = $1`), w.slug, findingType, subject)
	require.NoError(w.t, err)
	require.Contains(w.t, w.openFindings(findingType), subject)
	return subject
}

// forgetPaidPeriods makes a customer's charges look like rows written before
// charges named the period they paid for.
func (w *world) forgetPaidPeriods(customerID string) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE billing.payments SET metadata = metadata - 'period_start' WHERE customer_id = $1::uuid`), customerID)
	require.NoError(w.t, err)
}

// chargeAgain adds a real money fixture, either against the same obligation
// or a separate accepted adjustment in the same period. It never invents a
// current-price cadence to decide whether two captures duplicate money.
func (w *world) chargeAgain(customerID string, after time.Duration, separateBill bool) string {
	w.t.Helper()
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(w.t, err)
	ctx := merchant.WithID(w.t.Context(), w.client[embedded].MerchantID())
	var paymentID uuid.UUID
	require.NoError(w.t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		bound := d.NewWithPgxTx(tx)
		q := bound.Gen(ctx)
		var originalID uuid.UUID
		if err := tx.QueryRow(ctx, w.q(`SELECT id FROM billing.payments WHERE customer_id=$1::uuid AND status='succeeded' AND refunded_payment_id IS NULL ORDER BY purchased_at DESC LIMIT 1`), customerID).Scan(&originalID); err != nil {
			return err
		}
		service := payments.NewPaymentService(bound)
		original, err := service.GetByID(ctx, originalID)
		if err != nil {
			return err
		}
		payment := *original
		payment.ID, payment.TransactionID = uuid.New(), uuid.NewString()
		payment.PurchasedAt = original.PurchasedAt.Add(after)
		paymentID = payment.ID
		if !separateBill {
			return service.Create(ctx, &payment)
		}
		if original.OrderID == nil || original.SubscriptionID == nil || original.PspID == nil {
			return fmt.Errorf("fixture requires an accepted subscription order")
		}
		mid := w.client[embedded].MerchantID().UUID()
		sub, err := q.GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: mid, ID: *original.SubscriptionID})
		if err != nil {
			return err
		}
		lines, err := q.ListOrderLines(ctx, gen.ListOrderLinesParams{MerchantID: mid, OrderIds: []uuid.UUID{*original.OrderID}})
		if err != nil {
			return err
		}
		require.Len(w.t, lines, 1)
		l := lines[0]
		_, err = observed.Record(db.WithPSPID(ctx, *original.PspID), bound, observed.Bill{PSPID: *original.PspID, CustomerID: original.CustomerID, SubscriptionID: original.SubscriptionID,
			Owner: string(sub.CollectionPolicy), Kind: "adjustment", SourceKind: "fixture_adjustment", SourceKey: payment.ID.String(), Currency: payment.Currency, Total: payment.Amount,
			PeriodStart: *sub.CurrentPeriodStartsAt, PeriodEnd: *sub.CurrentPeriodEndsAt, Payment: &payment, Settled: true, SettlementSource: "cash",
			Lines: []observed.Line{{PriceID: l.PriceID, ProductID: l.ProductID, Description: l.Description, Quantity: models.DerefIntPtr(l.Quantity), UnitAmount: l.UnitAmount, Amount: l.Amount,
				BillingIntervalHours: models.DerefIntPtr(l.BillingIntervalHours), AccessDurationHours: models.DerefIntPtr(l.AccessDurationHours)}},
		}, w.clock.Now())
		return err
	}))
	return paymentID.String()
}

// dropLatestCharge removes the customer's latest charge row.
func (w *world) dropLatestCharge(customerID string) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`DELETE FROM billing.payments WHERE id = (SELECT id FROM billing.payments WHERE customer_id = $1::uuid ORDER BY purchased_at DESC LIMIT 1)`), customerID)
	require.NoError(w.t, err)
}
