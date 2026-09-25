//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

const duplicateCharge = "consistency.duplicate.provider_charge"

var cadences = []int{1, 24, 7 * 24, monthHours, 90 * 24, 365 * 24}

// Duplicate-charge findings follow each subscription's own periods, at every
// cadence: consecutive periods are never duplicates (the soak's hourly
// memberships were flagged by calendar month); two captured charges for one
// period are exactly one CRITICAL finding targeting the later charge; a
// decline and its successful retry are one charge; a price change starts a
// new coverage; a finding the scan no longer reports closes.
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
			retried.setDecline(visa.Last4, "insufficient_funds", "202")
			for i := range 3 {
				end := clean.periodEnd()
				clean.toPeriodEnd()
				w.runRenewals()
				require.True(t, clean.periodEnd().After(end))
				if i == 0 {
					require.Equal(t, "past_due", w.subscription(embedded, retried.sub).Status)
					retried.replaceCard(mastercard)
					w.runRenewals()
					require.Equal(t, "active", w.subscription(embedded, retried.sub).Status, "the retry renews")
				}
			}
			require.Len(t, completed(w.payments(embedded, clean.c.id)), 4)
			w.converge()
			require.Empty(t, w.openFindings(duplicateCharge), "consecutive periods and a declined-then-retried period are not duplicates")
			require.NotContains(t, w.openFindings(duplicateCharge), stale, "the calendar-month false positive closes")

			// A price change on the same subscription starts its own coverage.
			w.chargeAgain(clean.c.id, time.Minute, true)
			w.converge()
			require.Empty(t, w.openFindings(duplicateCharge), "a charge at a new price is not a duplicate")
			w.dropLatestCharge(clean.c.id)

			// A second capture of one period, a day later (across a month
			// boundary for long cadences): one CRITICAL finding naming the
			// subscription, targeting the later charge.
			later := w.chargeAgain(dup.c.id, 24*time.Hour, false)
			w.converge()
			findings := w.findings(duplicateCharge)
			require.Len(t, findings, 1)
			require.Equal(t, "critical", findings[0].severity)
			require.Contains(t, findings[0].subject, strings.TrimPrefix(dup.sub.String(), "sub_"))
			require.Contains(t, findings[0].action, "Refund the later charge pay_"+later, "the refund targets the later charge")
			w.dropLatestCharge(dup.c.id)
			w.converge()
			require.Empty(t, w.openFindings(duplicateCharge), "the finding closes once the duplicate is gone")

			// Rows written before charges named their period are judged by
			// the cadence.
			w.forgetPaidPeriods(clean.c.id)
			w.converge()
			require.Empty(t, w.openFindings(duplicateCharge))
			w.chargeAgain(clean.c.id, time.Minute, false)
			w.converge()
			require.Len(t, w.openFindings(duplicateCharge), 1, "two charges within one unnamed period")
		})
	}
}

type findingRow struct{ subject, severity, action string }

func (w *world) findings(findingType string) []findingRow {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT subject_key, severity, coalesce(recommended_action, '') FROM openrails.reconciliation_findings
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
	return strings.ReplaceAll(sql, "openrails.", pgx.Identifier{w.schema}.Sanitize()+".")
}

// seedFinding stands in for an open finding a previous detector raised.
func (w *world) seedFinding(findingType, subject string) string {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`INSERT INTO openrails.reconciliation_findings (merchant_id, finding_type, subject_key, severity, status)
		SELECT id, $2, $3, 'critical', 'requires_review' FROM openrails.merchants WHERE slug = $1`), w.slug, findingType, subject)
	require.NoError(w.t, err)
	require.Contains(w.t, w.openFindings(findingType), subject)
	return subject
}

// forgetPaidPeriods makes a customer's charges look like rows written before
// charges named the period they paid for.
func (w *world) forgetPaidPeriods(customerID string) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE openrails.payments SET metadata = metadata - 'period_start' WHERE customer_id = $1::uuid`), customerID)
	require.NoError(w.t, err)
}

// chargeAgain records a second provider capture of the customer's latest
// charge, after it, for the same period — or, newPrice, at another price of
// the product. It returns the new payment's id.
func (w *world) chargeAgain(customerID string, after time.Duration, newPrice bool) string {
	w.t.Helper()
	id := uuid.New()
	priceExpr, metadataExpr := "price_id", "metadata"
	if newPrice {
		// A plan change opens its own period at the moment it is paid.
		metadataExpr = `metadata || jsonb_build_object('period_start', to_char((purchased_at + $2::interval) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))`
		priceExpr = `(SELECT p2.id FROM openrails.prices p2 JOIN openrails.prices p1 ON p1.id = pay.price_id WHERE p2.product_id = p1.product_id AND p2.id <> p1.id LIMIT 1)`
		w.anotherPrice(customerID)
	}
	_, err := w.pool.Exec(w.t.Context(), w.q(`INSERT INTO openrails.payments (id, price_id, rail, transaction_id, amount, list_amount, currency, status, subscription_id,
			entitlements_spec_snapshot, metadata, purchased_at, created_at, merchant_id, customer_id, psp_id, attempt_kind, money_movement)
		SELECT $3::uuid, `+priceExpr+`, rail, transaction_id || '-again', amount, list_amount, currency, status, subscription_id,
			entitlements_spec_snapshot, `+metadataExpr+`, purchased_at + $2::interval, created_at, merchant_id, customer_id, psp_id, attempt_kind, money_movement
		FROM openrails.payments pay WHERE customer_id = $1::uuid AND status = 'completed' ORDER BY purchased_at DESC LIMIT 1`), customerID, fmt.Sprintf("%d seconds", int(after.Seconds())), id)
	require.NoError(w.t, err)
	return id.String()
}

// anotherPrice adds a second price to the product of the customer's latest
// charge (a plan change target).
func (w *world) anotherPrice(customerID string) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id, access_duration_hours, auto_renew, key)
		SELECT gen_random_uuid(), p.product_id, p.amount * 2, p.currency, p.merchant_id, p.access_duration_hours, p.auto_renew, p.key || '-plus'
		FROM openrails.payments pay JOIN openrails.prices p ON p.id = pay.price_id
		WHERE pay.customer_id = $1::uuid ORDER BY pay.purchased_at DESC LIMIT 1
		ON CONFLICT DO NOTHING`), customerID)
	require.NoError(w.t, err)
}

// dropLatestCharge removes the customer's latest charge row.
func (w *world) dropLatestCharge(customerID string) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`DELETE FROM openrails.payments WHERE id = (SELECT id FROM openrails.payments WHERE customer_id = $1::uuid ORDER BY purchased_at DESC LIMIT 1)`), customerID)
	require.NoError(w.t, err)
}
