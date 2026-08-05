//go:build integration

package subscriptions

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// #733: a declined renewal routed through FailMembership durably records a
// status='failed' payments row (attempt_kind=renewal, verbatim failure_code +
// normalized failure_reason), and the status-transition trigger records the
// active->past_due hop in the SAME transaction.
func TestFailMembership_RecordsFailedAttemptAndTransition(t *testing.T) {
	f := newFailopenFixture(t, 30*24, true)
	ctx := failopenCtx()
	sub, _ := f.create(t, models.RailNMI)

	code := "202"
	reason := "insufficient funds"
	require.NoError(t, f.lifecycle.FailMembership(ctx, &FailMembershipParams{
		Rail:                models.RailNMI,
		SubscriptionID:      &sub.ID,
		FailureReason:       &reason,
		FailureCode:         &code,
		RecordFailedAttempt: true,
	}))

	var status, attemptKind, failureCode, failureReason string
	var amount int64
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT status::text, attempt_kind, failure_code, failure_reason, amount
		 FROM openrails.payments WHERE subscription_id = $1 AND status = 'failed'`, sub.ID).
		Scan(&status, &attemptKind, &failureCode, &failureReason, &amount))
	require.Equal(t, "failed", status)
	require.Equal(t, "renewal", attemptKind)
	require.Equal(t, "202", failureCode)
	require.Equal(t, "insufficient_funds", failureReason)
	require.Equal(t, int64(9990000), amount)

	var fromStatus, toStatus string
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT from_status::text, to_status::text FROM openrails.subscription_status_transitions
		 WHERE subscription_id = $1 AND from_status = 'active'`, sub.ID).
		Scan(&fromStatus, &toStatus))
	require.Equal(t, "past_due", toStatus)

	// A second decline is a distinct attempt row (per-attempt idempotent id).
	require.NoError(t, f.lifecycle.FailMembership(ctx, &FailMembershipParams{
		Rail: models.RailNMI, SubscriptionID: &sub.ID,
		FailureCode: &code, RecordFailedAttempt: true,
	}))
	var failedRows int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM openrails.payments WHERE subscription_id = $1 AND status = 'failed'`, sub.ID).
		Scan(&failedRows))
	require.Equal(t, 2, failedRows)

	// Creation transition was recorded too (from NULL).
	var creationRows int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM openrails.subscription_status_transitions
		 WHERE subscription_id = $1 AND from_status IS NULL AND to_status = 'active'`, sub.ID).
		Scan(&creationRows))
	require.Equal(t, 1, creationRows)
}
