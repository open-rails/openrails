//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

type auditRow struct {
	decision, from, to string
	moved              bool // the paid period moved
}

// transitions is the subscription's lifecycle audit, oldest first.
func (w *world) transitions(sub string) []auditRow {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT coalesce(decision, ''), coalesce(from_status::text, ''), to_status::text,
		from_paid_through IS DISTINCT FROM to_paid_through FROM billing.subscription_status_transitions
		WHERE subscription_id = $1::uuid ORDER BY occurred_at, id`), sub)
	require.NoError(w.t, err)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (auditRow, error) {
		var a auditRow
		return a, r.Scan(&a.decision, &a.from, &a.to, &a.moved)
	})
	require.NoError(w.t, err)
	return out
}

// Every lifecycle decision is audited with its name, and a renewal that
// moves the paid period is recorded even when the status stays (#1102).
func TestLifecycleDecisionsAreAudited(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	e.toPeriodEnd()
	w.runRenewals()
	e.setDecline(visa.Last4, "insufficient_funds", "202")
	e.toPeriodEnd()
	w.runRenewals()
	require.Eventually(t, func() bool { return w.subscription(embedded, e.sub).Status == "past_due" }, 10*time.Second, 50*time.Millisecond)

	audit := w.transitions(subUUID(e.sub))
	var renewed, declined bool
	for _, a := range audit {
		require.NotEmpty(t, a.decision, "every transition names its decision: %+v", audit)
		renewed = renewed || (a.decision == "renewal_paid" && a.from == "active" && a.to == "active" && a.moved)
		declined = declined || (a.decision == "renewal_declined" && a.from == "active" && a.to == "past_due")
	}
	require.True(t, renewed, "the renewal that moved the period is audited: %+v", audit)
	require.True(t, declined, "the decline is audited: %+v", audit)
}

// A subscription's lifecycle audit is permanent, and every subscription has
// one from its first insert. Deleting the subscription, directly or through
// the merchant purge's per-table statement, is refused by the audit's foreign
// key (restrict_violation, which the purge reports as retained history). It
// used to cascade into the audit and fail inside its immutability trigger.
func TestSubscriptionWithLifecycleAuditIsNeverDeleted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	sub := subUUID(e.sub)
	before := w.transitions(sub)
	require.NotEmpty(t, before, "the insert itself is audited")

	refused := func(err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, "23001", pgErr.Code, pgErr.Message)
		require.Equal(t, "sst_subscription_fk", pgErr.ConstraintName)
	}
	_, err := w.pool.Exec(t.Context(), w.q(`DELETE FROM billing.subscriptions WHERE id = $1::uuid`), sub)
	refused(err)

	var merchantID string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT merchant_id::text FROM billing.subscriptions WHERE id = $1::uuid`), sub).Scan(&merchantID))
	tx, err := w.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(t.Context()) }()
	refused(gen.New(db.RewriteDBTX(tx, w.schema)).PurgeMerchantRowsSubscriptions(t.Context(), uuid.MustParse(merchantID)))
	require.NoError(t, tx.Rollback(t.Context()))

	require.Equal(t, before, w.transitions(sub), "the audit is untouched")
}
