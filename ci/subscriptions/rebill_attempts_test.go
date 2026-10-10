//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// cycleAttempt is one rebill attempt with its cycle.
type cycleAttempt struct {
	Kind, Owner, Source, ObservedVia, Category string
	Reason, TransactionID                      *string
	Cycle                                      uuid.UUID
	DueAt, AttemptedAt                         time.Time
}

func (w *world) cycleAttempts(sub billing.SubscriptionID) []cycleAttempt {
	w.t.Helper()
	schema := pgx.Identifier{w.schema}.Sanitize()
	rows, err := w.pool.Query(w.t.Context(), `SELECT a.kind, a.owner, a.source, a.observed_via, a.category, a.reason, a.transaction_id, c.id, c.due_at, a.attempted_at
		FROM `+schema+`.payment_attempts a JOIN `+schema+`.rebill_cycles c ON c.merchant_id = a.merchant_id AND c.id = a.cycle_id
		WHERE c.subscription_id = $1 ORDER BY a.attempted_at, a.id`, sub.UUID())
	require.NoError(w.t, err)
	defer rows.Close()
	var out []cycleAttempt
	for rows.Next() {
		var a cycleAttempt
		require.NoError(w.t, rows.Scan(&a.Kind, &a.Owner, &a.Source, &a.ObservedVia, &a.Category, &a.Reason, &a.TransactionID, &a.Cycle, &a.DueAt, &a.AttemptedAt))
		out = append(out, a)
	}
	require.NoError(w.t, rows.Err())
	return out
}

// An engine renewal declines twice and is collected on the second retry: one
// cycle, its first attempt the rebill, then two dunning retries.
func TestEngineRebillAttempts(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, rail, embedded)
			e.refreshBeforePeriodEnd()
			e.setDecline(visa.Last4, "insufficient_funds", "202")
			due := e.periodEnd()
			e.toPeriodEnd()
			w.runRenewals()
			for i := range 2 {
				if i == 1 {
					e.setDecline(visa.Last4, "", "")
				}
				sub := w.subscription(e.tp, e.sub)
				require.NotNil(t, nextRetry(sub))
				w.advanceHealthyTo(nextRetry(sub).Add(time.Second))
				w.runRenewals()
			}
			require.Equal(t, billing.SubscriptionActive, w.subscription(e.tp, e.sub).Status)

			rows := w.cycleAttempts(e.sub)
			require.Len(t, rows, 3)
			kinds := []string{rows[0].Kind, rows[1].Kind, rows[2].Kind}
			require.Equal(t, []string{"rebill", "dunning_retry", "dunning_retry"}, kinds)
			require.Equal(t, []string{"issuer_soft", "issuer_soft", "approved"}, []string{rows[0].Category, rows[1].Category, rows[2].Category})
			for _, a := range rows {
				require.Equal(t, rows[0].Cycle, a.Cycle, "one cycle")
				require.True(t, due.Equal(a.DueAt), "the cycle is the period that came due")
				require.Equal(t, []string{"engine", "openrails", "response"}, []string{a.Owner, a.Source, a.ObservedVia})
				require.NotEmpty(t, str(a.TransactionID))
			}
		})
	}
}

// The member pays a declined renewal themselves: a customer retry.
func TestEngineRebillCustomerRetry(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	e.refreshBeforePeriodEnd()
	e.setDecline(visa.Last4, "", "202")
	e.toPeriodEnd()
	w.runRenewals()
	require.Equal(t, billing.SubscriptionPastDue, w.subscription(e.tp, e.sub).Status)
	e.setDecline(visa.Last4, "", "")
	e.c.must(http.MethodPost, "/subscriptions/"+e.sub.String()+"/retry-now", "retry-"+uuid.NewString(), map[string]any{})
	w.settle()
	require.Equal(t, billing.SubscriptionActive, w.subscription(e.tp, e.sub).Status)
	rows := w.cycleAttempts(e.sub)
	require.Len(t, rows, 2)
	require.Equal(t, []string{"rebill", "customer_retry"}, []string{rows[0].Kind, rows[1].Kind})
	require.Equal(t, "approved", rows[1].Category)
	require.Equal(t, rows[0].Cycle, rows[1].Cycle)
}

// NMI declines its own scheduled charge and OpenRails' retry collects it: the
// NMI charge is the cycle's rebill, observed through the webhook, and the
// retry is OpenRails' dunning.
func TestNMIScheduleRebillAttempts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor)
	w.converge()
	w.nmi.SetDecline(visa.Last4, "202")
	due := l.periodEnd()
	w.advanceHealthyTo(due.Add(time.Hour))
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(false)))
	w.settle()
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionPastDue, sub.Status)
	w.nmi.SetDecline(visa.Last4, "")
	w.advanceHealthyTo(nextRetry(sub).Add(time.Second))
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status)

	rows := w.cycleAttempts(l.sub)
	require.Len(t, rows, 2)
	nmi, retry := rows[0], rows[1]
	require.Equal(t, []string{"rebill", "nmi_schedule", "provider_schedule", "webhook", "issuer_soft", "insufficient_funds"},
		[]string{nmi.Kind, nmi.Owner, nmi.Source, nmi.ObservedVia, nmi.Category, str(nmi.Reason)})
	require.Equal(t, w.nmi.LastDecline().TransactionID, str(nmi.TransactionID))
	require.Equal(t, []string{"dunning_retry", "nmi_schedule", "openrails", "approved"}, []string{retry.Kind, retry.Owner, retry.Source, retry.Category})
	require.Equal(t, nmi.Cycle, retry.Cycle)
	require.True(t, due.Equal(nmi.DueAt))
}
