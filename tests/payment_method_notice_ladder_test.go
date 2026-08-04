//go:build integration

package tests

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// or#870 bucket 2, the NOTIFICATION LADDER, end to end through the production
// mechanism: the real PaymentMethodNoticeWorker against the real table, the
// real work queue and the real notification_queue.
//
// Bucket 2 stops charging deliberately, which removes the only clock the
// customer was ever on: no charge attempt, no failure event, no notification.
// One notice at the decline and then silence is the failure mode the doctrine
// names by name. These tests are what say the reminders actually arrive.
//
// The invariant every one of them re-checks: the ladder is notification-only.
// However many rungs it sends, and however it ends, the subscription is still
// parked with access intact and the stored payment method untouched.

type noticeLadderRow struct {
	RungsSent    int64
	NextNoticeAt *time.Time
	ResolvedAt   *time.Time
	Resolution   *string
	ParkedAt     time.Time
	FailureCode  *string
}

func readNoticeLadder(t *testing.T, suite *TestContainerSuite, subscriptionID uuid.UUID) (noticeLadderRow, bool) {
	t.Helper()
	var row noticeLadderRow
	err := suite.Pool.QueryRow(suite.MerchantCtx(), `
		SELECT rungs_sent, next_notice_at, resolved_at, resolution, parked_at, failure_code
		  FROM openrails.payment_method_notices
		 WHERE subscription_id = $1`, subscriptionID).
		Scan(&row.RungsSent, &row.NextNoticeAt, &row.ResolvedAt, &row.Resolution, &row.ParkedAt, &row.FailureCode)
	if err != nil {
		return noticeLadderRow{}, false
	}
	return row, true
}

// updateRequiredNotices returns the bucket-2 notices this customer has, newest
// last, with the rung each one announced.
func updateRequiredNotices(t *testing.T, suite *TestContainerSuite, customerID uuid.UUID) []map[string]any {
	t.Helper()
	rows, err := suite.Pool.Query(suite.MerchantCtx(), `
		SELECT data FROM openrails.notification_queue
		 WHERE customer_id = $1 AND event_type = $2
		 ORDER BY created_at, id`, customerID, string(models.NotificationPaymentMethodUpdateRequired))
	require.NoError(t, err)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var data map[string]any
		require.NoError(t, rows.Scan(&data))
		out = append(out, data)
	}
	require.NoError(t, rows.Err())
	return out
}

// runNoticeLadderAt runs the REAL worker at a chosen moment.
func runNoticeLadderAt(t *testing.T, suite *TestContainerSuite, at time.Time) {
	t.Helper()
	w := &riverjobs.PaymentMethodNoticeWorker{
		DB:     suite.App.Runtime.DB,
		Clock:  clockwork.NewFakeClockAt(at),
		Config: suite.App.Runtime.Config,
	}
	require.NoError(t, w.Work(suite.WorkerCtx(), nil))
}

// assertStillParkedAndIntact is the invariant that outranks every rung.
func assertStillParkedAndIntact(t *testing.T, suite *TestContainerSuite, sub *models.Subscription, pm *models.PaymentMethod) {
	t.Helper()
	updated := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusUnknown, updated.Status, "the ladder must not move the subscription's status")
	assert.Nil(t, updated.CancelledAt, "the ladder must never cancel")
	assert.Nil(t, updated.NextRetryAt, "bucket 2 stays stopped: the ladder must not resume charging")

	ents := suite.QueryEntitlements(suite.MerchantCtx(), "WHERE source_id = $1 AND revoked_at IS NULL", sub.ID)
	assert.NotEmpty(t, ents, "the ladder must never cost the customer access")

	rowPresent, vaultDeletes := storedPaymentMethodDestruction(t, suite, pm.ID)
	assert.True(t, rowPresent, "the ladder must never delete a stored payment method")
	assert.Zero(t, vaultDeletes, "the ladder must never queue a stored-payment-method delete")
}

// The whole ladder, rung by rung: opened by the decline, walked by the worker,
// closed by running out — and nothing else changes at any step.
func TestOr870Bucket2LadderWalksEveryRungAndChangesNothingElse(t *testing.T) {
	suite := getSharedTestSuite(t)
	_, sub, pm := declineFixture(t, suite, 0)

	failWithCode(t, suite, sub, "223") // expired card

	// The decline opens the ladder with rung 1 already sent.
	ladder, ok := readNoticeLadder(t, suite, sub.ID)
	require.True(t, ok, "a bucket-2 park must open a notice ladder — otherwise the customer hears nothing again, ever")
	require.EqualValues(t, 1, ladder.RungsSent, "the decline itself is rung 1")
	require.NotNil(t, ladder.NextNoticeAt, "an open ladder must have its next rung scheduled")
	require.Nil(t, ladder.ResolvedAt)
	assert.Equal(t, "223", derefOrEmpty(ladder.FailureCode), "the ladder records the rail's code verbatim")
	parked := ladder.ParkedAt
	assert.WithinDuration(t, parked.Add(3*24*time.Hour), *ladder.NextNoticeAt, time.Second,
		"rung 2 falls due 3 days after the PARK, not 3 days after some later event")

	notices := updateRequiredNotices(t, suite, sub.CustomerID)
	require.Len(t, notices, 1, "the decline sends exactly one notice")
	assert.EqualValues(t, 1, notices[0]["rung"])
	assert.Equal(t, false, notices[0]["final"])

	// Not due yet: a worker pass before the rung is a no-op. This is what stops
	// an hourly worker from mailing a parked customer every hour.
	runNoticeLadderAt(t, suite, parked.Add(6*time.Hour))
	assert.Len(t, updateRequiredNotices(t, suite, sub.CustomerID), 1, "no rung is due yet")
	assertStillParkedAndIntact(t, suite, sub, pm)

	// Rung 2, at +3d.
	runNoticeLadderAt(t, suite, parked.Add(3*24*time.Hour+time.Minute))
	notices = updateRequiredNotices(t, suite, sub.CustomerID)
	require.Len(t, notices, 2, "rung 2 must be sent once its moment arrives")
	assert.EqualValues(t, 2, notices[1]["rung"])
	assert.Equal(t, false, notices[1]["final"], "rung 2 of 3 is not the last word")

	ladder, _ = readNoticeLadder(t, suite, sub.ID)
	assert.EqualValues(t, 2, ladder.RungsSent)
	require.NotNil(t, ladder.NextNoticeAt)
	assert.WithinDuration(t, parked.Add(10*24*time.Hour), *ladder.NextNoticeAt, time.Second,
		"rung 3 is anchored to the park too — a late worker must not push the rest of the ladder out")
	assert.Nil(t, ladder.ResolvedAt, "the ladder is not finished")
	assertStillParkedAndIntact(t, suite, sub, pm)

	// IDEMPOTENCE: the same pass again, at the same moment, sends nothing more.
	// The rung advance commits with the notification, so a re-run has no due row
	// to claim — this is the property that makes an hourly worker safe.
	runNoticeLadderAt(t, suite, parked.Add(3*24*time.Hour+2*time.Minute))
	assert.Len(t, updateRequiredNotices(t, suite, sub.CustomerID), 2, "a rung must be sent exactly once")

	// Rung 3, the final notice — and the ladder closes.
	runNoticeLadderAt(t, suite, parked.Add(10*24*time.Hour+time.Minute))
	notices = updateRequiredNotices(t, suite, sub.CustomerID)
	require.Len(t, notices, 3)
	assert.EqualValues(t, 3, notices[2]["rung"])
	assert.Equal(t, true, notices[2]["final"], "the last rung says it is the last one")

	ladder, _ = readNoticeLadder(t, suite, sub.ID)
	assert.EqualValues(t, 3, ladder.RungsSent)
	assert.Nil(t, ladder.NextNoticeAt, "a spent ladder has nothing left to schedule")
	require.NotNil(t, ladder.ResolvedAt)
	require.NotNil(t, ladder.Resolution)
	assert.Equal(t, "exhausted", *ladder.Resolution)

	// THE POINT: running out of reminders is not evidence of anything, so it
	// terminates nothing. The customer still has their subscription, their
	// access and their saved card.
	assertStillParkedAndIntact(t, suite, sub, pm)

	// And long after: a closed ladder never speaks again.
	runNoticeLadderAt(t, suite, parked.Add(90*24*time.Hour))
	assert.Len(t, updateRequiredNotices(t, suite, sub.CustomerID), 3, "an exhausted ladder is silent forever")
	assertStillParkedAndIntact(t, suite, sub, pm)
}

// The recovery path — the outcome bucket 2 exists to produce. Once the customer
// fixes the card, the reminders must stop immediately: asking someone to update
// a card they already updated is how a recovery email becomes a support ticket.
func TestOr870Bucket2LadderStopsWhenTheCustomerFixesTheCard(t *testing.T) {
	suite := getSharedTestSuite(t)
	_, sub, pm := declineFixture(t, suite, 0)

	failWithCode(t, suite, sub, "224") // invalid expiration date
	ladder, ok := readNoticeLadder(t, suite, sub.ID)
	require.True(t, ok)
	parked := ladder.ParkedAt

	// The card is fixed and collection resumes — the subscription leaves the
	// parked state by the ordinary path.
	_, err := suite.Pool.Exec(suite.MerchantCtx(),
		`UPDATE openrails.subscriptions SET status = 'active' WHERE id = $1`, sub.ID)
	require.NoError(t, err)

	runNoticeLadderAt(t, suite, parked.Add(3*24*time.Hour+time.Minute))

	assert.Len(t, updateRequiredNotices(t, suite, sub.CustomerID), 1,
		"a recovered customer must not be nagged about a card they already fixed")
	ladder, _ = readNoticeLadder(t, suite, sub.ID)
	require.NotNil(t, ladder.Resolution)
	assert.Equal(t, "recovered", *ladder.Resolution)
	assert.Nil(t, ladder.NextNoticeAt)
	assert.EqualValues(t, 1, ladder.RungsSent, "resolving is not sending")

	rowPresent, vaultDeletes := storedPaymentMethodDestruction(t, suite, pm.ID)
	assert.True(t, rowPresent)
	assert.Zero(t, vaultDeletes)
}

// If the subscription ends by some other route, the ladder stops too. A "please
// update your card" notice on a cancelled subscription is worse than silence.
func TestOr870Bucket2LadderStopsWhenTheSubscriptionEnds(t *testing.T) {
	suite := getSharedTestSuite(t)
	_, sub, _ := declineFixture(t, suite, 0)

	failWithCode(t, suite, sub, "225") // invalid card security code
	ladder, ok := readNoticeLadder(t, suite, sub.ID)
	require.True(t, ok)
	parked := ladder.ParkedAt

	now := time.Now().UTC()
	_, err := suite.Pool.Exec(suite.MerchantCtx(), `
		UPDATE openrails.subscriptions
		   SET status = 'cancelled', cancelled_at = $2, ended_at = $2,
		       cancel_type = 'user_requested', next_retry_at = NULL, grace_ends_at = NULL
		 WHERE id = $1`, sub.ID, now)
	require.NoError(t, err)

	runNoticeLadderAt(t, suite, parked.Add(3*24*time.Hour+time.Minute))

	assert.Len(t, updateRequiredNotices(t, suite, sub.CustomerID), 1,
		"a cancelled subscription must not receive a fix-your-card reminder")
	ladder, _ = readNoticeLadder(t, suite, sub.ID)
	require.NotNil(t, ladder.Resolution)
	assert.Equal(t, "ended", *ladder.Resolution)
}

// A second bucket-2 decline is a NEW problem, so the rungs re-anchor. Without
// this, a customer who fixed their card in March and whose replacement expired
// in September would inherit a spent ladder and be told once, never again.
func TestOr870Bucket2LadderRestartsOnAFreshDecline(t *testing.T) {
	suite := getSharedTestSuite(t)
	_, sub, _ := declineFixture(t, suite, 0)

	failWithCode(t, suite, sub, "223")
	first, ok := readNoticeLadder(t, suite, sub.ID)
	require.True(t, ok)

	// Walk it to exhaustion.
	runNoticeLadderAt(t, suite, first.ParkedAt.Add(3*24*time.Hour+time.Minute))
	runNoticeLadderAt(t, suite, first.ParkedAt.Add(10*24*time.Hour+time.Minute))
	spent, _ := readNoticeLadder(t, suite, sub.ID)
	require.NotNil(t, spent.Resolution)
	require.Equal(t, "exhausted", *spent.Resolution)

	// The customer fixes the card, collection resumes, and a later renewal
	// declines for a different bucket-2 reason.
	_, err := suite.Pool.Exec(suite.MerchantCtx(),
		`UPDATE openrails.subscriptions SET status = 'past_due' WHERE id = $1`, sub.ID)
	require.NoError(t, err)
	failWithCode(t, suite, sub, "201") // do not honor

	restarted, _ := readNoticeLadder(t, suite, sub.ID)
	assert.EqualValues(t, 1, restarted.RungsSent, "a fresh decline restarts the ladder at rung 1")
	assert.Nil(t, restarted.ResolvedAt, "a restarted ladder is open again")
	require.NotNil(t, restarted.NextNoticeAt)
	assert.Equal(t, "201", derefOrEmpty(restarted.FailureCode), "the ladder carries the NEW code")
	assert.True(t, restarted.ParkedAt.After(first.ParkedAt), "the rungs re-anchor on the new park")
}

// The ladder is a work QUEUE, not a scan (Paul's standing law: work scales with
// activity, not records). The cross-merchant fan-out must return merchants that
// actually have a due rung — and nobody else.
func TestOr870NoticeLadderFanOutIsDueWorkOnly(t *testing.T) {
	suite := getSharedTestSuite(t)
	_, sub, _ := declineFixture(t, suite, 0)

	failWithCode(t, suite, sub, "226") // invalid PIN
	ladder, ok := readNoticeLadder(t, suite, sub.ID)
	require.True(t, ok)

	dueMerchants := func(at time.Time) int {
		var n int
		require.NoError(t, suite.Pool.QueryRow(suite.WorkerCtx(),
			`SELECT count(*) FROM openrails.due_payment_method_notice_merchant_ids($1, 500)`, at).Scan(&n))
		return n
	}

	assert.Zero(t, dueMerchants(ladder.ParkedAt.Add(time.Hour)),
		"a parked customer whose next rung is days away is NOT due work")
	assert.Positive(t, dueMerchants(ladder.ParkedAt.Add(3*24*time.Hour+time.Minute)),
		"a merchant with a due rung must appear in the fan-out")

	// Walk it out, then confirm a closed ladder leaves the queue entirely.
	runNoticeLadderAt(t, suite, ladder.ParkedAt.Add(3*24*time.Hour+time.Minute))
	runNoticeLadderAt(t, suite, ladder.ParkedAt.Add(10*24*time.Hour+time.Minute))
	assert.Zero(t, dueMerchants(ladder.ParkedAt.Add(365*24*time.Hour)),
		"a resolved ladder must leave the work queue; otherwise the sweep grows with history")

	// And the merchant scoping is real: the work queue is SECURITY DEFINER over
	// every merchant, but the rows themselves stay policy-protected.
	var visible int
	require.NoError(t, suite.Pool.QueryRow(suite.MerchantCtx(),
		`SELECT count(*) FROM openrails.payment_method_notices WHERE merchant_id <> $1`,
		dbtest.TestMerchantID.UUID()).Scan(&visible))
	assert.Zero(t, visible, "RLS must hide every other merchant's ladder rows")
}

func derefOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
