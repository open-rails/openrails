//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// A paid period admits no second attempt, whatever writer tries: the database
// refuses a later attempt for a period whose attempt succeeded. The attempt
// number never makes a second charge legal.
func TestPaidPeriodAdmitsNoSecondAttempt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.refreshBeforePeriodEnd()
	e.toPeriodEnd()
	w.runRenewals()
	w.until(func() bool { return e.periodEnd().After(end) }, "the period renews")
	_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.provider_intents
		(merchant_id, rail, intent_type, subscription_id, payload, idempotency_key, status, attempts, next_attempt_at, origin, psp_id)
		SELECT merchant_id, rail, intent_type, subscription_id, jsonb_set(payload, '{attempt}', to_jsonb((payload->>'attempt')::int + 1)),
		       idempotency_key || ':again', 'pending', 0, now(), origin, psp_id
		FROM billing.provider_intents WHERE intent_type = 'subscription_collection' AND subscription_id = $1 AND status = 'succeeded'`), e.sub.UUID())
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "a second attempt for a paid period is refused")
	require.Equal(t, "provider_intents_obligation_unreleased_key", pgErr.ConstraintName)
	require.Len(t, e.providerLedger(), 2)
}
