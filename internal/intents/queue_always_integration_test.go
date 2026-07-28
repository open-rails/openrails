//go:build integration

package intents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #679 queue-always: a terminal FailMembership under provider_write_mode=limited
// still durably queues the nmi_delete intent (marker + intent atomic in the
// cancellation tx); the intent EXECUTOR is what parks it (system-origin under
// limited), and it drains once the mode allows.
func TestFailMembershipLimitedModeQueuesDeleteIntent(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	subID, productID, priceID := uuid.New(), uuid.New(), uuid.New()
	psid := "psid-" + uuid.NewString()[:8]
	custID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())
	now := time.Now().UTC()
	tenantID := dbtest.TestMerchantID.UUID()

	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "qa-prod-"+uuid.NewString()[:8], tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 999, 'USD', 720, true, $3)`, priceID, productID, tenantID)
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'past_due', 'nmi', $4, $5, $6, $5, $7, $8)`,
		subID, priceID, productID, psid, now.Add(-40*24*time.Hour), now.Add(-10*24*time.Hour), custID, tenantID)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.rail_mutation_logs
			WHERE rail_intent_id IN (SELECT id FROM openrails.rail_intents WHERE subscription_id = $1)`, subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", custID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	// Terminal dunning failure with the service in LIMITED mode.
	lifecycle := subscriptions.NewSubscriptionLifecycleService(dbi, nil, nil, nil, nil, nil)
	lifecycle.SetConfig(limitedModeConfig())
	lifecycle.SetDeferredDeleteScheduler(NewNMIDeleteScheduler(dbi, nil, OriginSystem, "test: dunning exhaustion"))
	reason := "transaction_failure"
	require.NoError(t, lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:           models.RailNMI,
		SubscriptionID: &subID,
		FailureReason:  &reason,
		Terminal:       true,
	}))

	// The decision is durable: cancelled + marker + PENDING system-origin intent.
	var subStatus string
	var marker *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, deletion_scheduled_at FROM openrails.subscriptions WHERE id = $1`, subID).
		Scan(&subStatus, &marker))
	assert.Equal(t, "cancelled", subStatus)
	require.NotNil(t, marker, "limited mode must still record DeletionScheduledAt (#679 queue-always)")

	var intentID uuid.UUID
	var intentStatus, origin string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id, status, origin FROM openrails.rail_intents
		 WHERE subscription_id = $1 AND intent_type = $2`, subID, TypeNMIDeleteSubscription).
		Scan(&intentID, &intentStatus, &origin))
	assert.Equal(t, StatusPending, intentStatus)
	assert.Equal(t, string(OriginSystem), origin)

	// Execution is gated INSTEAD: under limited the executor parks the
	// system-origin delete — no provider traffic, intent stays pending.
	fake, client := newFakeNMI(t, psid, true)
	fx := intentFixture{db: dbi, store: NewStore(dbi), subID: subID, psid: psid, userID: custID}
	runnerFor := func(cfg *config.Config) *Runner {
		return &Runner{
			Store: fx.store,
			// The intent's provider is lower(sub.Rail) = "nmi".
			Registry: NewRegistry(NewNMIDeleteHandler(dbi, cfg, fakeNMIResolver{client: client}, nil)),
			Config:   cfg,
		}
	}
	_, err := runnerFor(limitedModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	got := fx.intent(t, intentID)
	assert.Equal(t, StatusPending, got.Status, "limited mode parks the queued delete, never executes it")
	require.NotNil(t, got.LastFailureReason)
	assert.Contains(t, *got.LastFailureReason, "mode=limited")
	assert.Zero(t, fake.queryCalls.Load(), "no provider traffic under limited")
	assert.Zero(t, fake.deleteCalls.Load())

	// Mode flips to full: the queue drains.
	_, err = pool.Exec(ctx, "UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", intentID)
	require.NoError(t, err)
	_, err = runnerFor(fullModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, fx.intent(t, intentID).Status)
	assert.EqualValues(t, 1, fake.deleteCalls.Load())
	assert.Nil(t, fx.deletionMarker(t), "success finalizes the marker")
}
