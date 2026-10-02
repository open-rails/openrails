//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// findingsAbout lists every recorded finding (any status, auto-fixed
// included) whose subject names the subscription.
func (w *world) findingsAbout(sub openrails.SubscriptionID) []string {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT finding_type || ':' || status FROM openrails.reconciliation_findings
		WHERE subject_key LIKE '%' || $1 || '%' ORDER BY created_at`), sub.UUID().String())
	require.NoError(w.t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(w.t, err)
	return out
}

type accessWindow struct {
	start time.Time
	end   *time.Time
}

// subscriptionWindows is a subscription's live (not deleted) subscription-sourced
// entitlement windows.
func (w *world) subscriptionWindows(sub openrails.SubscriptionID) []accessWindow {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT start_at, end_at FROM openrails.entitlements
		WHERE source_type = 'subscription' AND source_id = $1::uuid AND deleted_at IS NULL AND revoked_at IS NULL ORDER BY start_at`), sub.UUID().String())
	require.NoError(w.t, err)
	defer rows.Close()
	var out []accessWindow
	for rows.Next() {
		var a accessWindow
		require.NoError(w.t, rows.Scan(&a.start, &a.end))
		out = append(out, a)
	}
	return out
}

// #1080 item 2: a takeover ends the legacy membership's access at the
// boundary in its own transaction; convergence finds nothing to repair, and
// the successor's first paid period grants from the boundary.
func TestNMIEngineTakeoverBoundsLegacyAccess(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importTakeoverLegacy(t, w, tp)
			end := l.periodEnd()
			done := l.takeover("takeover-" + uuid.NewString())
			require.Equal(t, "completed", done.Stage, "%+v", done)

			windows := w.subscriptionWindows(l.sub)
			require.NotEmpty(t, windows)
			for _, win := range windows {
				require.NotNil(t, win.end, "the legacy window is bounded by the takeover itself")
				require.False(t, win.end.After(end), "legacy access ends at the boundary")
			}
			w.converge()
			require.Empty(t, w.findingsAbout(l.sub), "nothing for convergence to repair on the legacy membership")
			require.Empty(t, w.findingsAbout(*done.SuccessorSubscriptionID))
			require.True(t, l.c.entitled(l.ent))

			w.advance(end.Sub(w.clock.Now()) + time.Hour)
			w.runRenewals()
			w.until(func() bool { return w.subscription(tp, *done.SuccessorSubscriptionID).CurrentPeriodEndsAt.After(end) }, "the first engine renewal")
			require.True(t, l.c.entitled(l.ent), "access continues from the boundary")
			successor := w.subscriptionWindows(*done.SuccessorSubscriptionID)
			require.NotEmpty(t, successor)
			require.True(t, successor[0].start.Equal(end), "the successor grants from the boundary (%s vs %s)", successor[0].start, end)
			w.converge()
			require.Empty(t, w.findingsAbout(l.sub))
			require.Empty(t, w.findingsAbout(*done.SuccessorSubscriptionID))
		})
	}
}

// #1080 item 4: importing a legacy book writes its access as the import's own
// effect; no finding is raised (and resolved) for any imported subscription.
func TestLegacyImportRaisesNoFindings(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			now := w.clock.Now()
			monthly := w.bookTier("monthly", 999, 30)
			b := w.newLegacyBook()
			active := b.add(&bookRow{source: "active", tier: monthly, paid: now.Add(10 * day), declared: true})
			cancelled := b.add(&bookRow{source: "cancelled", tier: monthly, paid: now.Add(20 * day), declared: true,
				cancel: openrails.CancelEvidence{Kind: "user_cancelled", At: now.Add(-5 * day)}})
			w.nmi.DeleteSchedule(cancelled.schedule)
			result, err := w.client[tp].ImportBilling(t.Context(), b.book)
			require.NoError(t, err)
			require.Len(t, result.Imported, 2, "%+v", result)
			w.settle()
			for _, r := range []*bookRow{active, cancelled} {
				sub := r.sub(w, tp)
				require.True(t, r.c.entitled(r.tier.ent), "%s is entitled without an operator converge", r.source)
				require.Empty(t, w.findingsAbout(sub.ID), "%s: no finding raised by the import", r.source)
			}
			w.converge()
			for _, r := range []*bookRow{active, cancelled} {
				require.Empty(t, w.findingsAbout(r.sub(w, tp).ID), "%s: convergence agrees with the import", r.source)
			}
		})
	}
}

// dashboardRefund refunds amount (minor units) of a charge outside OpenRails.
func (f *stripeFake) dashboardRefund(chargeID string, amount int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status, _ := f.createRefund(map[string][]string{"charge": {chargeID}, "amount": {fmt.Sprint(amount)}})
	if status != http.StatusOK {
		panic(fmt.Sprintf("stripe fake dashboard refund: %d", status))
	}
}

func (w *world) setProviderRefundAccess(policy string) {
	w.t.Helper()
	require.NoError(w.t, w.client[embedded].SetMerchantSettings(w.t.Context(), openrails.MerchantSettings{ProviderRefundAccess: &policy}))
}

// #1080 item 5: a refund made in the provider's own dashboard follows the
// merchant's provider_refund_access policy on every rail. Default: a full
// refund ends the charge's access (an NMI-billed membership also has its
// schedule deleted once; while disarmed the delete is held behind a finding),
// a partial refund keeps it; keep never revokes.
func TestProviderDashboardRefundAccessPolicy(t *testing.T) {
	t.Parallel()
	type row struct {
		name, kind, policy string
		full, armed        bool
		revoked            bool
	}
	rows := []row{
		{"legacy_full_default", "legacy", "", true, true, true},
		{"legacy_partial_default", "legacy", "", false, true, false},
		{"legacy_full_keep", "legacy", openrails.ProviderRefundKeep, true, true, false},
		{"legacy_partial_any", "legacy", openrails.ProviderRefundRevokeOnAny, false, true, true},
		{"legacy_full_disarmed", "legacy", "", true, false, true},
		{"nmi_engine_full_default", "nmi", "", true, true, true},
		{"nmi_engine_partial_default", "nmi", "", false, true, false},
		{"stripe_engine_full_default", "stripe", "", true, true, true},
		{"stripe_engine_full_keep", "stripe", openrails.ProviderRefundKeep, true, true, false},
	}
	for i, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			tp := []topology{embedded, remote}[i%2]
			w := newWorld(t)
			if r.armed {
				w.armDestructive()
			}
			if r.policy != "" {
				w.setProviderRefundAccess(r.policy)
			}
			var (
				c       *customer
				ent     string
				sub     openrails.SubscriptionID
				railSub string
			)
			rail := "nmi"
			switch r.kind {
			case "legacy":
				l := importLegacy(t, w, "nmi", tp)
				c, ent, sub, railSub = l.c, l.ent, l.sub, l.railSub
				tx := w.nmi.ledger(l.railCust)[0]
				w.nmi.Refund(tx.ID, map[bool]int64{true: tx.Amount, false: tx.Amount / 2}[r.full])
			case "nmi", "stripe":
				rail = r.kind
				e := enroll(t, w, r.kind, tp)
				c, ent, sub = e.c, e.ent, e.sub
				paid := e.providerLedger()[0]
				amount := map[bool]int64{true: paid.Amount, false: paid.Amount / 2}[r.full]
				if r.kind == "stripe" {
					w.stripe.dashboardRefund(paid.Charge, amount)
				} else {
					w.nmi.Refund(paid.ID, amount)
				}
			}
			require.True(t, c.entitled(ent))
			require.Equal(t, http.StatusOK, w.deliver(rail, w.refundNotice(rail)))
			w.advance(time.Hour)
			w.wake()
			w.converge()

			state := w.subscription(tp, sub)
			require.Equal(t, !r.revoked, c.entitled(ent), "access follows the policy")
			if r.revoked {
				require.Equal(t, "cancelled", state.Status)
			} else {
				require.NotEqual(t, "cancelled", state.Status)
			}
			if r.kind == "legacy" {
				switch {
				case r.revoked && r.armed:
					require.Equal(t, 1, w.nmi.ScheduleDeletes(railSub), "the NMI schedule ends exactly once")
				case r.revoked:
					require.Zero(t, w.nmi.ScheduleDeletes(railSub), "the delete waits for the switch")
					require.Contains(t, w.openFindings(providerCancelHeld), sub.UUID().String())
					w.armDestructive()
					w.advance(time.Hour)
					w.wake()
					require.Equal(t, 1, w.nmi.ScheduleDeletes(railSub), "the held delete runs once armed")
				default:
					require.Zero(t, w.nmi.ScheduleDeletes(railSub))
					require.True(t, w.nmi.ScheduleLive(railSub))
				}
			}
			// A later NMI or pull pass does not restore revoked access.
			w.converge()
			require.Equal(t, !r.revoked, c.entitled(ent), "revocation holds through convergence")
			require.Zero(t, len(w.nmi.Attempts())-map[bool]int{true: 1, false: 0}[r.kind == "nmi"], "no charge caused by the refund")
		})
	}
}
