//go:build integration

package tests

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// or#870 decline doctrine, end to end through the PRODUCTION chokepoint —
// SubscriptionLifecycleService.FailMembership on the runtime's own instance,
// with the real DeferredDeleteScheduler wired. Every "did this destroy a stored
// payment method?" assertion below is answered by the production mechanism (the
// payment_methods table and the nmi_vault_delete intent ledger), never a mock.
//
// The standing rule these tests exist to defend: OpenRails NEVER deletes a
// stored payment method. Not on expiry, not on a stolen card, not on
// cancellation, not ever.

// storedPaymentMethodDestruction counts everything that could have destroyed a
// customer's saved instrument: the local row disappearing, and any
// nmi_vault_delete intent on the ledger (the ONLY durable path that deletes a
// vault at the rail).
func storedPaymentMethodDestruction(t *testing.T, suite *TestContainerSuite, pmID uuid.UUID) (rowPresent bool, vaultDeleteIntents int) {
	t.Helper()
	ctx := suite.MerchantCtx()
	require.NoError(t, suite.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM openrails.payment_methods WHERE id = $1)`,
		pmID).Scan(&rowPresent))
	require.NoError(t, suite.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM openrails.rail_intents WHERE intent_type = $1`,
		intents.TypeNMIVaultDelete).Scan(&vaultDeleteIntents))
	return rowPresent, vaultDeleteIntents
}

func notificationEventTypes(t *testing.T, suite *TestContainerSuite, customerID uuid.UUID) []string {
	t.Helper()
	ctx := suite.MerchantCtx()
	rows, err := suite.Pool.Query(ctx, `
		SELECT event_type FROM openrails.notification_queue
		WHERE customer_id = $1
		ORDER BY created_at`, customerID)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var et string
		require.NoError(t, rows.Scan(&et))
		out = append(out, et)
	}
	require.NoError(t, rows.Err())
	return out
}

// declineFixture builds a past_due NMI subscription with a stored payment
// method, an active entitlement, and `priorFailures` recorded dunning attempts.
func declineFixture(t *testing.T, suite *TestContainerSuite, priorFailures int) (userID string, sub *models.Subscription, pm *models.PaymentMethod) {
	t.Helper()
	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userID = uuid.New().String()
	pm = suite.CreateTestPaymentMethod(userID)

	pastRetry := time.Now().Add(-1 * time.Hour)
	periodEnd := time.Now().Add(-2 * 24 * time.Hour)
	attempts := priorFailures

	sub = suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:              userID,
		PriceID:             priceID,
		Status:              models.StatusPastDue,
		Rail:                models.RailNMI,
		PaymentMethodID:     &pm.ID,
		RetryAttempts:       &attempts,
		NextRetryAt:         &pastRetry,
		PeriodStart:         periodEnd.Add(-30 * 24 * time.Hour),
		CurrentPeriodEndsAt: &periodEnd,
	})
	suite.CreateTestEntitlement(userID, "premium", &sub.ID, models.EntitlementSourceSubscription)
	return userID, sub, pm
}

func failWithCode(t *testing.T, suite *TestContainerSuite, sub *models.Subscription, code string) {
	t.Helper()
	outcome := collection.ClassifyDecline(string(models.RailNMI), code)
	certainty := ""
	if outcome == collection.DeclineNonRecoverable {
		certainty = collection.CertaintyNonRetryableDecline
	}
	reason := "rebill declined"
	require.NoError(t, suite.App.Runtime.SubscriptionLifecycleService.FailMembership(suite.MerchantCtx(),
		&subscriptions.FailMembershipParams{
			Rail:                models.RailNMI,
			SubscriptionID:      &sub.ID,
			FailureReason:       &reason,
			FailureCode:         &code,
			Decline:             outcome,
			RecordFailedAttempt: true,
			TerminalCertainty:   certainty,
		}))
}

// BUCKET 2 — their card, fixable. 223 expired card.
// 0 cancellations, 0 deletes, entitlements retained, notification emitted.
//
// Before or#870 this code was a "hard decline": it cancelled the subscription
// outright and revoked the customer's access on the FIRST failure, for a card
// they could have fixed in a minute.
func TestOr870Bucket2ExpiredCardKeepsAccessAndDeletesNothing(t *testing.T) {
	suite := getSharedTestSuite(t)
	_, sub, pm := declineFixture(t, suite, 0)

	failWithCode(t, suite, sub, "223") // expired card

	updated := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusUnknown, updated.Status,
		"bucket 2 parks as unknown — charging stops, the subscription lives on")
	assert.NotEqual(t, models.StatusCancelled, updated.Status, "bucket 2 must never cancel")
	assert.Nil(t, updated.CancelledAt, "bucket 2 must never cancel")
	assert.Nil(t, updated.NextRetryAt, "bucket 2 stops charging: no next attempt may be scheduled")
	assert.Nil(t, updated.DeletionScheduledAt, "bucket 2 queues no provider deletion of any kind")

	// Access retained: an unknown auto-renew subscription still projects
	// standing access (#691), and no entitlement was revoked.
	ents := suite.QueryEntitlements(suite.MerchantCtx(),
		"WHERE source_id = $1 AND revoked_at IS NULL", sub.ID)
	assert.NotEmpty(t, ents, "bucket 2 must retain the customer's entitlements")

	// The stored payment method is untouched.
	rowPresent, vaultDeletes := storedPaymentMethodDestruction(t, suite, pm.ID)
	assert.True(t, rowPresent, "bucket 2 must not delete the stored payment method")
	assert.Zero(t, vaultDeletes, "bucket 2 must not queue a stored-payment-method delete")

	// And no rail-side subscription cancellation either.
	cancelIntents, _, _ := queryNMIDeleteIntents(t, suite, sub.ID)
	assert.Zero(t, cancelIntents, "bucket 2 must not cancel at the rail")

	assert.Contains(t, notificationEventTypes(t, suite, sub.CustomerID),
		string(models.NotificationPaymentMethodUpdateRequired),
		"bucket 2 must tell the customer to update their payment method")
}

// Every bucket-2 code behaves identically — the whole point of collapsing the
// two classifiers is that no code in this set is left with a different answer.
func TestOr870Bucket2CoversEveryFixableCode(t *testing.T) {
	suite := getSharedTestSuite(t)
	// 250/251 (pick-up/lost card) sit here by owner decision (or#870, 2026-07-29):
	// the instrument is dead but the customer did nothing wrong and a reissued
	// card works, so losing a wallet must not cost a subscription.
	for _, code := range []string{"201", "204", "220", "221", "222", "223", "224", "225", "226", "240", "250", "251", "263", "461"} {
		t.Run(code, func(t *testing.T) {
			_, sub, pm := declineFixture(t, suite, 0)
			failWithCode(t, suite, sub, code)

			updated := suite.GetSubscription(sub.ID)
			require.Equal(t, models.StatusUnknown, updated.Status, "code %s must park, not cancel", code)
			rowPresent, _ := storedPaymentMethodDestruction(t, suite, pm.ID)
			require.True(t, rowPresent, "code %s must not destroy the stored payment method", code)
		})
	}
}

// BUCKET 3 — non-recoverable. 261 stop all recurring payments.
// The rail cancel happens, and ZERO stored payment methods are destroyed.
func TestOr870Bucket3CancelsAtTheRailAndDeletesNoPaymentMethod(t *testing.T) {
	suite := getSharedTestSuite(t)
	_, sub, pm := declineFixture(t, suite, 0)

	failWithCode(t, suite, sub, "261") // issuer withdrew the recurring mandate

	updated := suite.GetSubscription(sub.ID)
	require.Equal(t, models.StatusCancelled, updated.Status, "bucket 3 terminates on the first failure")
	require.NotNil(t, updated.CancelType)
	assert.Equal(t, models.CancelTypeExpired, *updated.CancelType)

	// The rail-side RECURRING SCHEDULE is cancelled, via the real deferred
	// delete scheduler — this is the nmi_delete_subscription intent, not a
	// payment-method delete.
	cancelIntents, _, _ := queryNMIDeleteIntents(t, suite, sub.ID)
	assert.GreaterOrEqual(t, cancelIntents, 1,
		"bucket 3 must cancel the recurring schedule at the rail")

	// THE STANDING RULE: the customer's stored card survives a stolen-card /
	// mandate-withdrawn cancellation. Only they delete it.
	rowPresent, vaultDeletes := storedPaymentMethodDestruction(t, suite, pm.ID)
	assert.True(t, rowPresent,
		"bucket 3 cancels the subscription, NEVER the stored payment method")
	assert.Zero(t, vaultDeletes,
		"bucket 3 must queue ZERO stored-payment-method deletes")

	assert.Contains(t, notificationEventTypes(t, suite, sub.CustomerID),
		string(models.NotificationPremiumEnded),
		"bucket 3 must tell the customer the subscription ended")

	var reason string
	require.NoError(t, suite.Pool.QueryRow(suite.MerchantCtx(), `
		SELECT COALESCE(data->>'reason', '') FROM openrails.notification_queue
		WHERE event_type = $1 AND customer_id = $2
		ORDER BY created_at DESC LIMIT 1`,
		models.NotificationPremiumEnded, sub.CustomerID).Scan(&reason))
	assert.Equal(t, string(subscriptions.PremiumEndReasonNonRecoverable), reason,
		"bucket 3 gets the re-subscribe copy, not the generic expiry copy")
}

func TestOr870Bucket3CoversEveryNonRecoverableCode(t *testing.T) {
	suite := getSharedTestSuite(t)
	for _, code := range []string{"252", "253", "261", "262"} {
		t.Run(code, func(t *testing.T) {
			_, sub, pm := declineFixture(t, suite, 0)
			failWithCode(t, suite, sub, code)

			updated := suite.GetSubscription(sub.ID)
			require.Equal(t, models.StatusCancelled, updated.Status, "code %s must terminate", code)
			rowPresent, vaultDeletes := storedPaymentMethodDestruction(t, suite, pm.ID)
			require.True(t, rowPresent, "code %s must not destroy the stored payment method", code)
			require.Zero(t, vaultDeletes, "code %s must queue no vault delete", code)
		})
	}
}

// BUCKET 1 — ours or transient. The schedule continues, and the ladder fires:
// "still trying" now, "we gave up" when the schedule is exhausted.
func TestOr870Bucket1KeepsTheScheduleAndNotifiesBothRungs(t *testing.T) {
	suite := getSharedTestSuite(t)
	_, sub, pm := declineFixture(t, suite, 0)

	// Rung 1: still trying.
	failWithCode(t, suite, sub, "202") // insufficient funds

	updated := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusPastDue, updated.Status, "bucket 1 stays in dunning")
	require.NotNil(t, updated.NextRetryAt, "bucket 1 must schedule the next attempt")
	assert.True(t, updated.NextRetryAt.After(time.Now()), "the next attempt is in the future")
	require.NotNil(t, updated.RetryAttempts)
	assert.Equal(t, 1, *updated.RetryAttempts)

	assert.Contains(t, notificationEventTypes(t, suite, sub.CustomerID),
		string(models.NotificationPaymentMethodFailed),
		"bucket 1 must tell the customer it is failing and we will keep trying")
	assert.NotContains(t, notificationEventTypes(t, suite, sub.CustomerID),
		string(models.NotificationPremiumEnded),
		"bucket 1 must not announce the end while it is still retrying")

	// Rung 2: schedule exhausted, we gave up.
	maxFailures := collection.MaxFailures(30 * 24)
	for i := *updated.RetryAttempts; i < maxFailures; i++ {
		failWithCode(t, suite, sub, "202")
	}

	exhausted := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusCancelled, exhausted.Status,
		"bucket 1 terminates only when the schedule is genuinely exhausted")
	assert.Contains(t, notificationEventTypes(t, suite, sub.CustomerID),
		string(models.NotificationPremiumEnded),
		"bucket 1 must tell the customer we have given up")

	// Even after giving up: the card is theirs.
	rowPresent, vaultDeletes := storedPaymentMethodDestruction(t, suite, pm.ID)
	assert.True(t, rowPresent, "dunning exhaustion must not destroy the stored payment method")
	assert.Zero(t, vaultDeletes, "dunning exhaustion must queue no vault delete")
}

// An unrecognized code lands in bucket 1. Missing evidence must never cost a
// customer their subscription — the doctrine's load-bearing safety property,
// asserted here through the real lifecycle rather than only at the classifier.
func TestOr870UnknownCodeIsBucket1(t *testing.T) {
	suite := getSharedTestSuite(t)
	_, sub, pm := declineFixture(t, suite, 0)

	failWithCode(t, suite, sub, "999") // not in NMI's published set

	updated := suite.GetSubscription(sub.ID)
	assert.Equal(t, models.StatusPastDue, updated.Status,
		"an unknown code keeps dunning; it never stops charging and never cancels")
	assert.NotNil(t, updated.NextRetryAt, "an unknown code keeps the retry schedule")

	rowPresent, vaultDeletes := storedPaymentMethodDestruction(t, suite, pm.ID)
	assert.True(t, rowPresent)
	assert.Zero(t, vaultDeletes)
}

// The invariant that outranks every bucket: no automated path — dunning,
// cancellation, reconcile — produces a stored-payment-method delete. The ONLY
// producer of TypeNMIVaultDelete is the authenticated user route.
func TestOr870NoAutomatedPathEverDeletesAStoredPaymentMethod(t *testing.T) {
	suite := getSharedTestSuite(t)

	for _, code := range []string{"202", "223", "252", "261", "999"} {
		_, sub, _ := declineFixture(t, suite, 0)
		failWithCode(t, suite, sub, code)
	}
	// And a full dunning exhaustion for good measure.
	_, sub, _ := declineFixture(t, suite, collection.MaxFailures(30*24)-1)
	failWithCode(t, suite, sub, "202")

	var vaultDeletes int
	require.NoError(t, suite.Pool.QueryRow(suite.MerchantCtx(), `
		SELECT COUNT(*) FROM openrails.rail_intents WHERE intent_type = $1`,
		intents.TypeNMIVaultDelete).Scan(&vaultDeletes))
	assert.Zero(t, vaultDeletes,
		"no decline outcome, and no dunning exhaustion, may ever queue a stored-payment-method delete")
}
