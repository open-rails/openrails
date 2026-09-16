//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/require"
)

func (fx *upgradeAdoptFixture) upgrade(t *testing.T) (*CheckoutResponse, error) {
	t.Helper()
	return fx.svc.processUpgrade(fx.ctx, fx.req, fx.user, fx.newPrice, fx.newProduct, fx.existingSub, fx.target)
}
func (fx *upgradeAdoptFixture) operation(t *testing.T) gen.OpenrailsRailIntent {
	t.Helper()
	key := NMIUpgradeIdempotencyKey(fx.svc.getUpgradeIdempotencyKey(fx.req, fx.user.ID, fx.existingSub.ID, fx.newPrice.ID))
	in, err := intents.NewStore(fx.db).GetByIdempotencyKey(fx.ctx, key)
	require.NoError(t, err)
	return in
}
func (fx *upgradeAdoptFixture) restartAndVerify(t *testing.T) gen.OpenrailsRailIntent {
	t.Helper()
	in := fx.operation(t)
	_, err := fx.db.Qx(fx.ctx).Exec(fx.ctx, `UPDATE openrails.rail_intents SET next_attempt_at='epoch',claimed_until=NULL WHERE id=$1`, in.ID)
	require.NoError(t, err)
	// New runner/handler/request cache: only the database survives the restart.
	fx.svc.IdempotencyService = newStatefulIdemStub()
	runner := &intents.Runner{Store: intents.NewStore(fx.db), Registry: intents.NewRegistry(NewNMIUpgradeIntentHandler(fx.svc)), Config: fullModeConfig(), Clock: fx.svc.Clock()}
	fx.svc.Intents = runner
	_, err = runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	return fx.operation(t)
}
func (fx *upgradeAdoptFixture) positiveProration() {
	fx.newPrice.Amount = 60_000_000
	fx.existingSub.Price.Amount = 0
}

func TestUpgradeReceiptsRestartAfterLocalRollback(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.positiveProration()
	fx.newProduct.EntitlementsSpec = map[string]*int{"upgraded_access": nil}
	// Reject only this successor insert, after the old-subscription UPDATE.
	// The failed transaction must preserve old access and all provider receipts.
	constraint := "test_upgrade_" + fx.newPrice.ID.String()[:8]
	_, err := fx.db.Qx(fx.ctx).Exec(fx.ctx, fmt.Sprintf(`ALTER TABLE openrails.subscriptions ADD CONSTRAINT %s CHECK (price_id <> '%s'::uuid)`, constraint, fx.newPrice.ID))
	require.NoError(t, err)
	remove := func() {
		_, err := fx.db.Qx(fx.ctx).Exec(fx.ctx, `ALTER TABLE openrails.subscriptions DROP CONSTRAINT IF EXISTS `+constraint)
		require.NoError(t, err)
	}
	t.Cleanup(remove)
	_, err = fx.upgrade(t)
	require.ErrorIs(t, err, ErrCheckoutProcessing)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	in := fx.operation(t)
	require.Equal(t, intents.StatusUnknownNeedsVerify, in.Status)
	var journal nmiUpgradeProgress
	require.NoError(t, json.Unmarshal(in.ResultEvidence, &journal))
	require.NotNil(t, journal.Successor.Enrollment)
	require.NotNil(t, journal.Proration.Sale)
	old, err := fx.svc.SubscriptionService.GetByID(fx.ctx, fx.existingSub.ID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, old.Status)
	var count int
	require.NoError(t, fx.db.Qx(fx.ctx).QueryRow(fx.ctx, `SELECT count(*) FROM openrails.payments WHERE customer_id=$1`, old.CustomerID).Scan(&count))
	require.Zero(t, count)
	remove()
	// Replay cannot consult mutable provider credentials or recalculate pricing.
	fx.svc.ResolveNMIClientOverride = func(context.Context, string) (*nmi.NMIClient, error) {
		return nil, errors.New("provider unavailable after receipt")
	}
	fx.newPrice.Amount = 999_000_000
	result := fx.restartAndVerify(t)
	require.Equal(t, intents.StatusSucceeded, result.Status)
	response, err := fx.upgrade(t)
	require.NoError(t, err)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	next, err := fx.svc.SubscriptionService.GetByID(fx.ctx, *response.SubscriptionID)
	require.NoError(t, err)
	var payload NMIUpgradePayload
	require.NoError(t, json.Unmarshal(result.Payload, &payload))
	require.Equal(t, payload.PeriodEnd, *next.CurrentPeriodEndsAt)
	var amount int64
	require.NoError(t, fx.db.Qx(fx.ctx).QueryRow(fx.ctx, `SELECT count(*),min(amount) FROM openrails.payments WHERE subscription_id=$1`, next.ID).Scan(&count, &amount))
	require.Equal(t, 1, count)
	require.Equal(t, payload.ProrationAmount, amount)
	require.NoError(t, fx.db.Qx(fx.ctx).QueryRow(fx.ctx, `SELECT count(*) FROM openrails.entitlements WHERE source_id=$1`, next.ID).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, fx.db.Qx(fx.ctx).QueryRow(fx.ctx, `SELECT count(*) FROM openrails.rail_intents WHERE subscription_id=$1 AND intent_type=$2`, old.ID, intents.TypeNMIDeleteSubscription).Scan(&count))
	require.Equal(t, 1, count)
}

func TestUpgradeProrationLostResponseConvergesOnlyOnReceipt(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.positiveProration()
	fx.gateway.saleMode.Store("ambiguousHidden")
	_, err := fx.upgrade(t)
	require.ErrorIs(t, err, ErrCheckoutProcessing)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Equal(t, intents.StatusUnknownNeedsVerify, fx.restartAndVerify(t).Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load(), "empty query never resends")
	fx.gateway.saleVisible.Store(true)
	require.Equal(t, intents.StatusSucceeded, fx.restartAndVerify(t).Status)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
}

func TestUpgradeSuccessorLostResponseDoesNotAdoptRosterSimilarity(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.gateway.createMode.Store("ambiguousLanded")
	_, err := fx.upgrade(t)
	require.ErrorIs(t, err, ErrCheckoutProcessing)
	require.Equal(t, intents.StatusUnknownNeedsVerify, fx.restartAndVerify(t).Status)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	old, err := fx.svc.SubscriptionService.GetByID(fx.ctx, fx.existingSub.ID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, old.Status)
}

func TestUpgradeDefinitiveProrationRefusalQueuesSuccessorCancellation(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.positiveProration()
	fx.gateway.saleMode.Store("decline")
	_, err := fx.upgrade(t)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCheckoutProcessing)
	in := fx.operation(t)
	require.Equal(t, intents.StatusFailedTerminal, in.Status)
	var p NMIUpgradePayload
	require.NoError(t, json.Unmarshal(in.Payload, &p))
	next, err := fx.svc.SubscriptionService.GetByID(fx.ctx, p.NewSubscriptionID)
	require.NoError(t, err)
	require.Equal(t, models.StatusCancelled, next.Status)
	require.NotNil(t, next.DeletionScheduledAt)
	old, err := fx.svc.SubscriptionService.GetByID(fx.ctx, p.OldSubscriptionID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, old.Status)
	_, err = fx.upgrade(t)
	require.Error(t, err)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	deletion, err := intents.NewStore(fx.db).GetByIdempotencyKey(fx.ctx, intents.NMIDeleteIdempotencyKey(p.NewSubscriptionID))
	require.NoError(t, err)
	require.Equal(t, intents.StatusPending, deletion.Status)
	require.NotEqual(t, uuid.Nil, deletion.ID)
}

func TestUpgradeSuccessorReceiptResumesOnlyUnsentProration(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.positiveProration()
	runner := fx.svc.Intents.(*intents.Runner)
	runner.Registry = intents.NewRegistry() // a worker rolling to the new handler
	_, err := fx.upgrade(t)
	require.ErrorIs(t, err, ErrCheckoutProcessing)
	in := fx.operation(t)
	constraint := "test_step_" + in.ID.String()[:8]
	_, err = fx.db.Qx(fx.ctx).Exec(fx.ctx, fmt.Sprintf(`ALTER TABLE openrails.rail_intents ADD CONSTRAINT %s CHECK (id <> '%s'::uuid OR NOT (coalesce(result_evidence,'{}') ? 'proration'))`, constraint, in.ID))
	require.NoError(t, err)
	remove := func() {
		_, err := fx.db.Qx(fx.ctx).Exec(fx.ctx, `ALTER TABLE openrails.rail_intents DROP CONSTRAINT IF EXISTS `+constraint)
		require.NoError(t, err)
	}
	t.Cleanup(remove)
	runner.Registry = intents.NewRegistry(NewNMIUpgradeIntentHandler(fx.svc))
	_, err = fx.upgrade(t)
	require.ErrorIs(t, err, ErrCheckoutProcessing)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	require.Zero(t, fx.gateway.saleCalls.Load(), "write-ahead failure precedes provider submission")
	remove()
	fx.newPrice.Amount = 999_000_000
	resumed := fx.restartAndVerify(t)
	require.Equal(t, intents.StatusFailedRetryable, resumed.Status, "the verifier does not submit the unsent step")
	response, err := fx.upgrade(t)
	require.NoError(t, err)
	require.Equal(t, "success", response.Status)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	var payload NMIUpgradePayload
	require.NoError(t, json.Unmarshal(fx.operation(t).Payload, &payload))
	require.EqualValues(t, 60_000_000, payload.ProrationAmount)
}
