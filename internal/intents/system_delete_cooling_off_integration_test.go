//go:build integration

package intents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// or#842: the last two gaps in the destructive-safety set, on the REAL terminal
// dunning path — a user cancel already had an undo window and the #732 ceiling;
// the automated paths, which queue the most irreversible work with no human in
// the loop, had neither.

type systemDeleteFixture struct {
	subID    uuid.UUID
	custID   uuid.UUID
	psid     string
	merchant uuid.UUID
}

func seedSystemDeleteSubscription(ctx context.Context, t *testing.T, dbi *db.DB) systemDeleteFixture {
	t.Helper()
	pool := dbi.Pool()
	f := systemDeleteFixture{
		subID:    uuid.New(),
		psid:     "psid-" + uuid.NewString()[:8],
		merchant: dbtest.TestMerchantID.UUID(),
	}
	f.custID = dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())
	productID, priceID := uuid.New(), uuid.New()
	now := time.Now().UTC()

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "cool-prod-"+uuid.NewString()[:8], f.merchant)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 999, 'USD', 720, true, $3)`, priceID, productID, f.merchant)
	// Period ended 10 days ago: the dominant automated shape (dunning exhausted
	// on a lapsed period), and no known future rebill to clamp the window to.
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'past_due', 'nmi', $4, $5, $6, $5, $7, $8)`,
		f.subID, priceID, productID, f.psid, now.Add(-40*24*time.Hour), now.Add(-10*24*time.Hour), f.custID, f.merchant)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.rail_mutation_logs
			WHERE rail_intent_id IN (SELECT id FROM openrails.rail_intents WHERE subscription_id = $1)`, f.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = $1", f.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", f.custID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", f.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})
	return f
}

// The automated delete is due AFTER a cooling-off window, not at `now`. The
// window is only useful because the handler's relevance check is authoritative:
// a row that stops being cancelled-awaiting-delete inside it supersedes the
// intent, so a terminal decision we got wrong never reaches the provider.
func TestSystemOriginDeleteHasACoolingOffWindow(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	f := seedSystemDeleteSubscription(ctx, t, dbi)
	before := time.Now().UTC()

	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, nil, nil, nil, nil, nil)
	lifecycle.SetConfig(fullModeConfig())
	lifecycle.SetDeferredDeleteScheduler(NewNMIDeleteScheduler(dbi, NewRateCeiling(dbi), OriginSystem, "test: dunning exhaustion"))
	reason := "transaction_failure"
	require.NoError(t, lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:              models.RailNMI,
		SubscriptionID:    &f.subID,
		FailureReason:     &reason,
		Terminal:          true,
		TerminalCertainty: collection.CertaintyDunningExhausted,
	}))

	var subStatus string
	var marker *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, deletion_scheduled_at FROM openrails.subscriptions WHERE id = $1`, f.subID).
		Scan(&subStatus, &marker))
	require.Equal(t, "cancelled", subStatus)
	require.NotNil(t, marker)

	var intentID uuid.UUID
	var dueAt time.Time
	var origin string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id, next_attempt_at, origin FROM openrails.rail_intents
		  WHERE subscription_id = $1 AND intent_type = $2`, f.subID, TypeNMIDeleteSubscription).
		Scan(&intentID, &dueAt, &origin))
	assert.Equal(t, string(OriginSystem), origin)

	// Marker and intent agree, and both sit a cooling-off window out — NOT `now`.
	assert.WithinDuration(t, before.Add(subscriptions.SystemDeleteCoolingOff), dueAt, time.Minute,
		"the automated delete is due immediately — an erroneous terminal decision reaches the provider before anything can notice")
	assert.WithinDuration(t, dueAt, marker.UTC(), time.Second, "marker and intent must name the same instant")
	assert.True(t, dueAt.After(before.Add(23*time.Hour)))

	// Nothing drains it while the window is open: the executor only claims due
	// intents, so no provider traffic happens at all.
	fake, client := newFakeNMI(t, f.psid, true)
	runner := &Runner{
		Store:    NewStore(dbi),
		Registry: NewRegistry(NewNMIDeleteHandler(dbi, fullModeConfig(), fakeNMIResolver{client: client}, nil)),
		Config:   fullModeConfig(),
	}
	_, err := runner.RunExecuteOnce(ctx)
	require.NoError(t, err)
	fx := intentFixture{db: dbi, store: NewStore(dbi), subID: f.subID, psid: f.psid, userID: f.custID}
	assert.Equal(t, StatusPending, fx.intent(t, intentID).Status, "an intent inside its cooling-off window is not due")
	assert.Zero(t, fake.deleteCalls.Load(), "the rail schedule was deleted inside the cooling-off window")

	// What the window BUYS: the subscription recovers inside it (a late charge, a
	// converge pass that resurrects the row, an operator undo). The delete is due
	// now — and supersedes itself instead of firing.
	_, err = pool.Exec(ctx,
		`UPDATE openrails.subscriptions SET status='active', deletion_scheduled_at=NULL WHERE id=$1`, f.subID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1`, intentID)
	require.NoError(t, err)
	_, err = runner.RunExecuteOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, StatusSuperseded, fx.intent(t, intentID).Status)
	assert.Zero(t, fake.deleteCalls.Load(), "a recovered subscription must never have its rail schedule deleted")
}

// The same terminal path, for a merchant whose AUTOMATION is already over its
// per-merchant hourly ceiling: the delete is refused at the producer, and
// because the intent enqueue shares the cancellation's transaction, the whole
// terminal decision rolls back — no cancel, no revoke, no queued delete.
func TestSystemOriginDeleteIsCountedByTheRateCeiling(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	f := seedSystemDeleteSubscription(ctx, t, dbi)

	// Fill this merchant's automated destructive budget for the rolling hour.
	seeded := make([]uuid.UUID, 0, PerMerchantSystemHourlyCeiling)
	for i := 0; i < PerMerchantSystemHourlyCeiling; i++ {
		id := uuid.New()
		_, err := pool.Exec(ctx,
			`INSERT INTO openrails.rail_intents
			   (id, merchant_id, rail, intent_type, idempotency_key, status, origin, next_attempt_at, created_at)
			 VALUES ($1, $2, 'mobius', $3, $4, 'pending', 'system', now(), now())`,
			id, f.merchant, TypeNMIVaultDelete, TypeNMIVaultDelete+":ceil:"+uuid.NewString())
		require.NoError(t, err)
		seeded = append(seeded, id)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM openrails.rail_intents WHERE id = ANY($1)`, seeded)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM openrails.reconciliation_findings WHERE merchant_id = $1 AND subject_key = $2`,
			f.merchant, "system:"+f.merchant.String())
	})

	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, nil, nil, nil, nil, nil)
	lifecycle.SetConfig(fullModeConfig())
	lifecycle.SetDeferredDeleteScheduler(NewNMIDeleteScheduler(dbi, NewRateCeiling(dbi), OriginSystem, "test: dunning exhaustion"))
	reason := "transaction_failure"
	err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:              models.RailNMI,
		SubscriptionID:    &f.subID,
		FailureReason:     &reason,
		Terminal:          true,
		TerminalCertainty: collection.CertaintyDunningExhausted,
	})
	require.Error(t, err, "an automated terminal cancel over the merchant's ceiling must be refused")
	require.True(t, errors.Is(err, ErrRateCeilingTripped))

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM openrails.subscriptions WHERE id = $1`, f.subID).Scan(&status))
	assert.Equal(t, "past_due", status, "the refused op must leave the customer's subscription untouched")

	var queued int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.rail_intents WHERE subscription_id = $1`, f.subID).Scan(&queued))
	assert.Zero(t, queued, "fail-closed: no write-ahead delete intent exists, so the delete cannot happen")

	// And the operator hears about it.
	var findings int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.reconciliation_findings
		  WHERE merchant_id = $1 AND finding_type = $2 AND subject_key = $3 AND status = 'requires_review'`,
		f.merchant, RateCeilingTrippedFindingType, "system:"+f.merchant.String()).Scan(&findings))
	assert.Equal(t, 1, findings)
}
