//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/open-rails/openrails/internal/integrations/nmi"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rejectTierTerminalWrite(t *testing.T, subscription uuid.UUID, kind, status string) func() {
	t.Helper()
	ddl := dbtest.SharedSuperuserPGXPool(t)
	name := "review_terminal_" + uuid.NewString()[:8]
	_, err := ddl.Exec(context.Background(), fmt.Sprintf(`CREATE FUNCTION billing.%s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected tier terminal failure'; END$$; CREATE TRIGGER %s BEFORE UPDATE ON billing.rail_intents FOR EACH ROW WHEN (NEW.subscription_id='%s'::uuid AND NEW.intent_type='%s' AND NEW.status='%s') EXECUTE FUNCTION billing.%s()`, name, name, subscription, kind, status, name))
	require.NoError(t, err)
	remove := func() {
		_, err := ddl.Exec(context.Background(), "DROP FUNCTION IF EXISTS billing."+name+"() CASCADE")
		require.NoError(t, err)
	}
	t.Cleanup(remove)
	return remove
}

func TestNMITierTerminalFailureRollsBackLocalEffects(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.positiveProration()
	fx.newProduct.EntitlementsSpec = map[string]*int{"new-tier-benefit": nil}
	removeTerminal := rejectTierTerminalWrite(t, fx.existingSub.ID, "nmi_upgrade", intents.StatusSucceeded)
	_, err := fx.upgrade(t)
	require.NoError(t, err)
	in := fx.operation(t)
	require.NotEqual(t, intents.StatusSucceeded, in.Status)
	_, paid, err := intents.LoadCollectedReceipt(in)
	require.NoError(t, err)
	require.True(t, paid, "exact paid custody survives the injected local terminal failure")
	p, err := subscriptions.DecodeNMIUpgradePayload(in)
	require.NoError(t, err)
	old, err := fx.svc.SubscriptionService.GetByID(fx.ctx, p.OldSubscriptionID)
	require.NoError(t, err)
	assert.Equal(t, "active", string(old.Status), "terminal failure must roll back predecessor cancellation")
	assert.Zero(t, fx.count(t, `SELECT count(*) FROM billing.subscriptions WHERE id=$1`, p.NewSubscriptionID), "successor must share terminal transaction")
	assert.Zero(t, fx.count(t, `SELECT count(*) FROM billing.payments WHERE id=$1`, p.NewPaymentID), "payment must share terminal transaction")
	assert.Zero(t, fx.count(t, `SELECT count(*) FROM billing.host_outbox WHERE payment_id=$1`, p.NewPaymentID), "payment settlement outbox must share terminal transaction")
	assert.Zero(t, fx.count(t, `SELECT count(*) FROM billing.entitlements WHERE source_id=$1`, p.NewSubscriptionID), "access must share terminal transaction")
	assert.Zero(t, fx.count(t, `SELECT count(*) FROM billing.rail_intents WHERE intent_type='nmi_delete_subscription' AND subscription_id=$1`, p.OldSubscriptionID), "old-provider cancellation must not escape terminal rollback")
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	removeTerminal()
	ddl := dbtest.SharedSuperuserPGXPool(t)
	constraint := "tier_outbox_" + uuid.NewString()[:8]
	_, err = ddl.Exec(fx.ctx, fmt.Sprintf(`ALTER TABLE billing.host_outbox ADD CONSTRAINT %s CHECK(payment_id <> '%s'::uuid)`, constraint, p.NewPaymentID))
	require.NoError(t, err)
	removeOutbox := func() {
		_, err := ddl.Exec(context.Background(), "ALTER TABLE billing.host_outbox DROP CONSTRAINT IF EXISTS "+constraint)
		require.NoError(t, err)
	}
	t.Cleanup(removeOutbox)
	require.Equal(t, intents.StatusUnknownNeedsVerify, fx.restartAndVerify(t).Status)
	require.Zero(t, fx.count(t, `SELECT count(*) FROM billing.payments WHERE id=$1`, p.NewPaymentID))
	require.Zero(t, fx.count(t, `SELECT count(*) FROM billing.subscriptions WHERE id=$1`, p.NewSubscriptionID))
	removeOutbox()
	fx.svc.ResolveNMIClientOverride = func(context.Context, string) (*nmi.NMIClient, error) {
		return nil, errors.New("provider offline after retained receipt")
	}
	current := fx.operation(t)
	start := make(chan struct{})
	results := make(chan intents.Outcome, 2)
	for range 2 {
		go func() { <-start; results <- NewNMIUpgradeIntentHandler(fx.svc).Verify(fx.ctx, current) }()
	}
	close(start)
	for range 2 {
		require.Equal(t, intents.OutcomeSucceeded, (<-results).Class)
	}
	require.Equal(t, intents.StatusSucceeded, fx.operation(t).Status)
	require.Equal(t, 1, fx.count(t, `SELECT count(*) FROM billing.payments WHERE id=$1`, p.NewPaymentID))
	require.Equal(t, 1, fx.count(t, `SELECT count(*) FROM billing.host_outbox WHERE payment_id=$1`, p.NewPaymentID))
	require.Equal(t, 1, fx.count(t, `SELECT count(*) FROM billing.entitlements WHERE source_id=$1`, p.NewSubscriptionID))
	require.Equal(t, 1, fx.count(t, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='nmi_delete_subscription'`, p.OldSubscriptionID))
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
}

func TestStripeTierTerminalFailureRollsBackLocalEffects(t *testing.T) {
	for _, action := range []string{"upgrade", "downgrade"} {
		t.Run(action, func(t *testing.T) {
			fx := newStripeTierFixture(t)
			target := fx.pro
			if action == "downgrade" {
				_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.subscriptions SET price_id=$2,product_id=$3 WHERE id=$1`, fx.sub.ID, fx.pro.ID, fx.pro.ProductID)
				require.NoError(t, err)
				fx.stripe.declare(fx.sub.RailSubscriptionID, "si_pro", fx.proRef, fx.sub.CurrentPeriodStartsAt.Unix(), fx.sub.CurrentPeriodEndsAt.Unix())
				target = fx.basic
			}
			before := fx.local(t)
			removeTerminal := rejectTierTerminalWrite(t, fx.sub.ID, "stripe_tier_change", intents.StatusSucceeded)
			key := "atomic-review-" + uuid.NewString()
			_, err := fx.change(key, target)
			require.NoError(t, err)
			in := fx.operation(key)
			require.NotEqual(t, intents.StatusSucceeded, in.Status)
			var progress stripeTierChangeProgress
			require.NoError(t, json.Unmarshal(in.ResultEvidence, &progress))
			if action == "upgrade" {
				require.NotNil(t, progress.Update.Subscription)
			} else {
				require.NotNil(t, progress.Phases.Schedule)
			}
			after := fx.local(t)
			assert.Equal(t, before.PriceID, after.PriceID, "price change must share operation terminal transaction")
			assert.Equal(t, before.ScheduledPriceID, after.ScheduledPriceID, "scheduled change must share operation terminal transaction")
			assert.True(t, after.CurrentPeriodEndsAt.Equal(*before.CurrentPeriodEndsAt), "period change must share terminal transaction")
			removeTerminal()
			fx.stripe.mu.Lock()
			requests := len(fx.stripe.requests)
			fx.stripe.mu.Unlock()
			current := fx.operation(key)
			h := NewStripeTierChangeIntentHandler(fx.svc)
			start := make(chan struct{})
			results := make(chan intents.Outcome, 2)
			for range 2 {
				go func() { <-start; results <- h.Verify(fx.ctx, current) }()
			}
			close(start)
			for range 2 {
				require.Equal(t, intents.OutcomeSucceeded, (<-results).Class)
			}
			require.Equal(t, intents.StatusSucceeded, fx.operation(key).Status)
			fx.stripe.mu.Lock()
			require.Equal(t, requests, len(fx.stripe.requests), "cached receipt completion performs no provider calls")
			fx.stripe.mu.Unlock()
			final := fx.local(t)
			if action == "upgrade" {
				require.Equal(t, target.ID, final.PriceID)
			} else {
				require.Equal(t, &target.ID, final.ScheduledPriceID)
			}

		})
	}
}

func TestStripeTerminalReplayDoesNotRestoreACancelledScheduledChange(t *testing.T) {
	fx := newStripeTierFixture(t)
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.subscriptions SET price_id=$2,product_id=$3 WHERE id=$1`, fx.sub.ID, fx.pro.ID, fx.pro.ProductID)
	require.NoError(t, err)
	fx.stripe.declare(fx.sub.RailSubscriptionID, "si_pro", fx.proRef, fx.sub.CurrentPeriodStartsAt.Unix(), fx.sub.CurrentPeriodEndsAt.Unix())
	key := "cancel-after-commit-" + uuid.NewString()
	_, err = fx.change(key, fx.basic)
	require.NoError(t, err)
	completed := fx.operation(key)
	require.Equal(t, intents.StatusSucceeded, completed.Status)
	// The provider's later cancellation is mirrored through the same locked repo
	// used by lifecycle commands. It preserves the active predecessor price.
	require.NoError(t, fx.db.MerchantTx(fx.ctx, func(ctx context.Context, tx pgx.Tx) error {
		repo := subscriptions.NewSubscriptionRepo(fx.db.NewWithPgxTx(tx))
		sub, err := repo.GetByIDForUpdate(ctx, fx.sub.ID)
		if err != nil {
			return err
		}
		sub.ScheduledPriceID = nil
		return repo.UpdateAt(ctx, sub, fx.clock.Now())
	}))
	require.Nil(t, fx.local(t).ScheduledPriceID)
	stale := completed
	stale.Status = intents.StatusInFlight
	require.Equal(t, intents.OutcomeSucceeded, NewStripeTierChangeIntentHandler(fx.svc).Verify(fx.ctx, stale).Class)
	require.Nil(t, fx.local(t).ScheduledPriceID, "a completed old operation cannot restore a later-cancelled schedule")
	require.Equal(t, fx.pro.ID, fx.local(t).PriceID)
}

func TestTierRefusalTerminalFailureRollsBackCleanup(t *testing.T) {
	t.Run("nmi", func(t *testing.T) {
		fx := newUpgradeAdoptFixture(t)
		fx.positiveProration()
		fx.gateway.saleMode.Store("decline")
		remove := rejectTierTerminalWrite(t, fx.existingSub.ID, "nmi_upgrade", intents.StatusFailedTerminal)
		_, err := fx.upgrade(t)
		require.NoError(t, err)
		in := fx.operation(t)
		require.Equal(t, intents.StatusUnknownNeedsVerify, in.Status)
		p, err := subscriptions.DecodeNMIUpgradePayload(in)
		require.NoError(t, err)
		require.Zero(t, fx.count(t, `SELECT count(*) FROM billing.subscriptions WHERE id=$1`, p.NewSubscriptionID))
		require.Zero(t, fx.count(t, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='nmi_delete_subscription'`, p.NewSubscriptionID))
		remove()
		require.Equal(t, intents.StatusFailedTerminal, fx.restartAndVerify(t).Status)
		require.Equal(t, 1, fx.count(t, `SELECT count(*) FROM billing.subscriptions WHERE id=$1`, p.NewSubscriptionID))
		require.Equal(t, 1, fx.count(t, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='nmi_delete_subscription'`, p.NewSubscriptionID))
		require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	})
	t.Run("stripe", func(t *testing.T) {
		fx := newStripeTierFixture(t)
		fx.stripe.setMode("decline")
		remove := rejectTierTerminalWrite(t, fx.sub.ID, "stripe_tier_change", intents.StatusFailedTerminal)
		key := "refusal-atomic-" + uuid.NewString()
		_, err := fx.change(key, fx.pro)
		require.NoError(t, err)
		require.Equal(t, intents.StatusUnknownNeedsVerify, fx.operation(key).Status)
		require.Equal(t, fx.basic.ID, fx.local(t).PriceID)
		remove()
		require.Equal(t, intents.StatusFailedTerminal, fx.verifyOnce(t, key).Status)
		require.Len(t, fx.stripe.posts("/v1/subscriptions/"), 1)
	})
}
