//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Money invariants checked when every world ends, whatever the test asserted.
// The providers' ledgers (the fakes) are the source of truth:
//
//	recorded:  every approved, unrefunded provider charge is a completed local
//	           payment or a settled invoice payment, unless its operation is
//	           still unresolved;
//	evidenced: every completed local rail payment is a provider charge;
//	once:      an order's net captures and non-cash settlement do not exceed
//	           its accepted total; separate adjustments and partials are valid,
//	           a provider's extra capture counts once its excess is flagged for
//	           refund review (pull.bill.overpaid), and a usage order's capture
//	           may exceed by less than one rail minor unit when that remainder
//	           is credited to its customer;
//	moved:     payments holds only money that moved: a declined charge is a
//	           payment attempt, never a failed payments row.
//
// A test whose scenario deliberately breaks one opts out with its reason.
type moneyInvariants struct{ waived map[string]string }

// waive disables one invariant for this world; the reason is the documentation.
func (w *world) waive(invariant, reason string) {
	w.t.Helper()
	if strings.TrimSpace(reason) == "" {
		w.t.Fatal("an invariant waiver needs a reason")
	}
	if w.invariants.waived == nil {
		w.invariants.waived = map[string]string{}
	}
	w.invariants.waived[invariant] = reason
}

func (w *world) checkMoneyInvariants() {
	t := w.t
	if t.Failed() || t.Skipped() {
		return
	}
	schema := pgx.Identifier{w.schema}.Sanitize()
	// t.Context is already canceled while cleanups run.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A Stripe charge is recorded by its own id or, as hosted Checkout
	// records it, by its PaymentIntent's.
	provider, intentOf, intents := map[string]bool{}, map[string]string{}, map[string]bool{}
	for _, e := range w.nmi.ledger("") {
		provider[e.ID] = e.Refunded < e.Amount
	}
	for _, e := range w.stripe.ledger("") {
		provider[e.Charge] = e.Refunded < e.Amount
		intentOf[e.Charge], intents[e.ID] = e.ID, true
	}

	local := map[string]bool{}
	rows, err := w.pool.Query(ctx, `SELECT transaction_id, rail FROM `+schema+`.payments
		WHERE channel = 'rail' AND status = 'succeeded' AND money_movement = 'rail' AND amount > 0 AND deleted_at IS NULL AND refunded_payment_id IS NULL`)
	if err != nil {
		t.Errorf("invariants: read payments: %v", err)
		return
	}
	var phantom []string
	for rows.Next() {
		var id, rail string
		if err := rows.Scan(&id, &rail); err != nil {
			t.Errorf("invariants: scan payment: %v", err)
			rows.Close()
			return
		}
		local[id] = true
		if (rail == "nmi" || rail == "stripe") && !hasKey(provider, id) && !intents[id] {
			phantom = append(phantom, rail+":"+id)
		}
	}
	rows.Close()
	unresolved := 0
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.provider_intents
		WHERE status IN ('in_flight', 'unknown_needs_verify', 'failed_retryable', 'pending')
		  AND intent_type IN ('subscription_collection', 'manual_rebill', 'initial_membership', 'nmi_sale', 'order_collection', 'nmi_upgrade', 'stripe_tier_change')`).Scan(&unresolved); err != nil {
		t.Errorf("invariants: read open operations: %v", err)
		return
	}
	var unrecorded []string
	if unresolved == 0 {
		for id, live := range provider {
			if live && !local[id] && !local[intentOf[id]] {
				unrecorded = append(unrecorded, id)
			}
		}
	}

	var twice []string
	// Each order whose net captures and non-cash settlement exceed its total,
	// unless the excess is a flagged provider extra capture; with the excess
	// and, for usage, the rounding credits its customer received.
	rows, err = w.pool.Query(ctx, `SELECT o.id::text, o.origin, o.currency, sum(p.amount) + coalesce(b.amount,0) - o.total,
		    coalesce((SELECT sum(g.amount) FROM `+schema+`.grants g
		        JOIN `+schema+`.ledger_transfers t ON t.merchant_id=g.merchant_id AND t.grant_id=g.id AND t.amount=g.amount
		        WHERE g.merchant_id=o.merchant_id AND g.customer_id=o.customer_id AND g.currency=o.currency
		          AND g.kind='credit' AND g.event='grant' AND g.spec_snapshot->'deposit'->>'source'='invoice_rounding_purchase'), 0)
		FROM `+schema+`.orders o
		JOIN `+schema+`.payments p ON p.merchant_id=o.merchant_id AND p.order_id=o.id
		LEFT JOIN (SELECT merchant_id, order_id, max(non_cash_amount) AS amount
		    FROM `+schema+`.provider_bills GROUP BY merchant_id, order_id) b
		  ON b.merchant_id=o.merchant_id AND b.order_id=o.id
		WHERE p.status IN ('succeeded','refunded') AND p.money_movement='rail' AND p.deleted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM `+schema+`.reconciliation_findings f
		      WHERE f.merchant_id=o.merchant_id AND f.finding_type='pull.bill.overpaid' AND f.subject_key='order:'||o.id::text)
		GROUP BY o.id, o.merchant_id, o.customer_id, o.origin, o.currency, o.total, b.amount
		HAVING sum(p.amount) + coalesce(b.amount,0) > o.total`)
	if err != nil {
		t.Errorf("invariants: read order settlements: %v", err)
		return
	}
	for rows.Next() {
		var key, origin, currency string
		var excess, credited int64
		if err := rows.Scan(&key, &origin, &currency, &excess, &credited); err != nil {
			t.Errorf("invariants: scan order settlement: %v", err)
			rows.Close()
			return
		}
		// A usage capture rounds up to the rail's minor unit; that remainder,
		// below one unit of the currency, is credited to the customer.
		unit, err := moneyutil.RailMinorToNative(currency, 1)
		if origin == "usage" && err == nil && excess < unit && credited >= excess {
			continue
		}
		twice = append(twice, key)
	}
	rows.Close()

	var failed []string
	rows, err = w.pool.Query(ctx, `SELECT id::text FROM `+schema+`.payments WHERE status = 'failed' AND refunded_payment_id IS NULL`)
	if err != nil {
		t.Errorf("invariants: read failed payments: %v", err)
		return
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			failed = append(failed, id)
		}
	}
	rows.Close()

	w.reportInvariant("evidenced", "local payments with no provider charge", phantom)
	w.reportInvariant("recorded", "provider charges with no local payment", unrecorded)
	w.reportInvariant("once", "order settlements exceed their accepted totals", twice)
	w.reportInvariant("moved", "failed charges in payments", failed)
}

func (w *world) reportInvariant(name, what string, violations []string) {
	if len(violations) == 0 {
		return
	}
	if _, ok := w.invariants.waived[name]; ok {
		return
	}
	sort.Strings(violations)
	w.t.Errorf("money invariant %q broken: %s: %v", name, what, violations)
}

func hasKey(m map[string]bool, k string) bool { _, ok := m[k]; return ok }
