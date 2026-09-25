//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Money invariants checked when every world ends, whatever the test asserted.
// The providers' ledgers (the fakes) are the source of truth:
//
//	recorded:  every approved, unrefunded provider charge is a completed local
//	           payment, unless its operation is still unresolved;
//	evidenced: every completed local rail payment is a provider charge;
//	once:      no subscription period holds two completed charges.
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
	// t.Context is already cancelled while cleanups run.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	provider := map[string]bool{}
	for _, e := range w.nmi.ledger("") {
		provider[e.ID] = e.Refunded < e.Amount
	}
	for _, e := range w.stripe.ledger("") {
		provider[e.Charge] = e.Refunded < e.Amount
	}

	local := map[string]bool{}
	rows, err := w.pool.Query(ctx, `SELECT transaction_id, rail FROM `+schema+`.payments
		WHERE status = 'completed' AND money_movement = 'rail' AND amount > 0 AND deleted_at IS NULL AND refunded_payment_id IS NULL`)
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
		if (rail == "nmi" || rail == "stripe") && !hasKey(provider, id) {
			phantom = append(phantom, rail+":"+id)
		}
	}
	rows.Close()

	unresolved := 0
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.rail_intents
		WHERE status IN ('in_flight', 'unknown_needs_verify', 'failed_retryable', 'pending')
		  AND intent_type IN ('subscription_collection', 'manual_rebill', 'initial_membership', 'nmi_sale', 'invoice_collection', 'nmi_upgrade', 'stripe_tier_change')`).Scan(&unresolved); err != nil {
		t.Errorf("invariants: read open operations: %v", err)
		return
	}
	var unrecorded []string
	if unresolved == 0 {
		for id, live := range provider {
			if live && !local[id] {
				unrecorded = append(unrecorded, id)
			}
		}
	}

	var twice []string
	rows, err = w.pool.Query(ctx, `SELECT subscription_id::text || '@' || (metadata->>'period_start') FROM `+schema+`.payments p
		WHERE status = 'completed' AND money_movement = 'rail' AND amount > 0 AND deleted_at IS NULL AND refunded_payment_id IS NULL
		  AND subscription_id IS NOT NULL AND metadata ? 'period_start'
		  AND NOT EXISTS (SELECT 1 FROM `+schema+`.payments r WHERE r.refunded_payment_id = p.id AND r.deleted_at IS NULL)
		GROUP BY subscription_id, metadata->>'period_start' HAVING count(*) > 1`)
	if err != nil {
		t.Errorf("invariants: read periods: %v", err)
		return
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err == nil {
			twice = append(twice, key)
		}
	}
	rows.Close()

	w.reportInvariant("evidenced", "local payments with no provider charge", phantom)
	w.reportInvariant("recorded", "provider charges with no local payment", unrecorded)
	w.reportInvariant("once", "subscription periods charged twice", twice)
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
