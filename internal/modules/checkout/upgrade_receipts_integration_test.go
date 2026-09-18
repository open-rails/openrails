//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/open-rails/openrails"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
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
	// New runner and handler: all recovery state lives in the database.
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
	ddl := dbtest.SharedSuperuserPGXPool(t)
	fx.positiveProration()
	fx.newProduct.EntitlementsSpec = map[string]*int{"upgraded_access": nil}
	// Reject only this successor insert, after the old-subscription UPDATE.
	// The failed transaction must preserve old access and all provider receipts.
	constraint := "test_upgrade_" + fx.newPrice.ID.String()[:8]
	_, err := ddl.Exec(fx.ctx, fmt.Sprintf(`ALTER TABLE openrails.subscriptions ADD CONSTRAINT %s CHECK (price_id <> '%s'::uuid)`, constraint, fx.newPrice.ID))
	require.NoError(t, err)
	remove := func() {
		_, err := ddl.Exec(fx.ctx, `ALTER TABLE openrails.subscriptions DROP CONSTRAINT IF EXISTS `+constraint)
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
	require.True(t, payload.PeriodEnd.Equal(*next.CurrentPeriodEndsAt), "recovered period preserves the frozen instant")
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
	ddl := dbtest.SharedSuperuserPGXPool(t)
	fx.positiveProration()
	runner := fx.svc.Intents.(*intents.Runner)
	runner.Registry = intents.NewRegistry() // a worker rolling to the new handler
	_, err := fx.upgrade(t)
	require.ErrorIs(t, err, ErrCheckoutProcessing)
	in := fx.operation(t)
	constraint := "test_step_" + in.ID.String()[:8]
	_, err = ddl.Exec(fx.ctx, fmt.Sprintf(`ALTER TABLE openrails.rail_intents ADD CONSTRAINT %s CHECK (id <> '%s'::uuid OR NOT (coalesce(result_evidence,'{}') ? 'proration'))`, constraint, in.ID))
	require.NoError(t, err)
	remove := func() {
		_, err := ddl.Exec(fx.ctx, `ALTER TABLE openrails.rail_intents DROP CONSTRAINT IF EXISTS `+constraint)
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

func TestUpgradePublicReplayUsesFrozenReceiptAfterCatalogArchive(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.positiveProration()
	first, err := fx.upgrade(t)
	require.NoError(t, err)
	_, err = fx.db.Qx(fx.ctx).Exec(fx.ctx, `UPDATE openrails.prices SET archived=true WHERE id=$1`, fx.newPrice.ID)
	require.NoError(t, err)
	request := &TierChangeRequest{SubscriptionID: fx.existingSub.ID, PriceID: openrails.PriceID(fx.newPrice.ID).String(), IdempotencyKey: fx.req.IdempotencyKey}
	replayed, err := fx.svc.TierChange(fx.ctx, request, fx.user)
	require.NoError(t, err, "the cancelled predecessor and archived price cannot strand a committed receipt")
	require.Equal(t, "succeeded", replayed.Status)
	require.NotNil(t, first.SubscriptionID)
	require.NotNil(t, replayed.SubscriptionID)
	require.EqualValues(t, 60_000_000, replayed.AmountDueNow)
	require.EqualValues(t, 60_000_000, replayed.NextChargeAmount)
	require.Equal(t, "USD", replayed.Currency)
	require.Equal(t, "nmi", replayed.Payment.Rail)
	require.Equal(t, first.TransactionID, replayed.Payment.TransactionID)
	_, err = fx.svc.TierChange(fx.ctx, request, &UserIdentity{ID: uuid.NewString()})
	var refused *TierChangeError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, http.StatusNotFound, refused.HTTPStatus)
	request.PriceID = uuid.NewString()
	_, err = fx.svc.TierChange(fx.ctx, request, fx.user)
	require.ErrorAs(t, err, &refused)
	require.Equal(t, http.StatusConflict, refused.HTTPStatus)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
}

func TestUpgradeUnresolvedPredecessorRejectsASecondRequest(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.gateway.createMode.Store("ambiguousLanded")
	_, err := fx.upgrade(t)
	require.ErrorIs(t, err, ErrCheckoutProcessing)
	fx.req.IdempotencyKey = uuid.NewString()
	_, err = fx.upgrade(t)
	require.ErrorIs(t, err, ErrTierChangePending)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load(), "a second request cannot bypass the first operation's unresolved submission")
}

func TestUpgradeWireAmountsUseFrozenMicros(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	key := NMIUpgradeIdempotencyKey(fx.svc.getUpgradeIdempotencyKey(fx.req, fx.user.ID, fx.existingSub.ID, fx.newPrice.ID))
	// A sub-cent price has no exact USD rail amount: refused before any
	// durable operation or provider request.
	fx.newPrice.Amount = 60_125_000
	_, err := fx.upgrade(t)
	require.ErrorContains(t, err, "not representable")
	_, err = intents.NewStore(fx.db).GetByIdempotencyKey(fx.ctx, key)
	require.True(t, db.IsNotFound(err), "no operation is frozen for an unrepresentable amount: %v", err)
	require.Zero(t, fx.gateway.createCalls.Load())
	require.Zero(t, fx.gateway.saleCalls.Load())

	// 696 of 720 hours remain on the $50.00 predecessor: credit ceil($48.3333)
	// = $48.34, so the legs differ and a swapped mapping cannot pass.
	fx.newPrice.Amount = 60_120_000
	_, err = fx.upgrade(t)
	require.NoError(t, err)
	var payload NMIUpgradePayload
	require.NoError(t, json.Unmarshal(fx.operation(t).Payload, &payload))
	require.EqualValues(t, 60_120_000, payload.RecurringAmount)
	require.EqualValues(t, 11_780_000, payload.ProrationAmount)
	require.Equal(t, "60.12", fx.gateway.recurringAmount.Load(), "60,120,000 USD micros enrolls a $60.12 recurring schedule")
	require.Equal(t, "11.78", fx.gateway.saleAmount.Load(), "11,780,000 USD micros submits an $11.78 proration sale")

	fx.newPrice.Amount = 999_000_000
	_, err = fx.upgrade(t)
	require.NoError(t, err)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
}

func (fx *upgradeAdoptFixture) resolve(t *testing.T, resolution intents.Resolution) (gen.OpenrailsRailIntent, error) {
	t.Helper()
	return fx.svc.Intents.(*intents.Runner).Resolve(fx.ctx, fx.operation(t).ID, resolution)
}

func (fx *upgradeAdoptFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, fx.db.Qx(fx.ctx).QueryRow(fx.ctx, query, args...).Scan(&n))
	return n
}

// A landed successor whose response was lost is not adopted from roster
// similarity. Only the exact subscription id, read back on the frozen vault and
// plan, resolves it; the executor then submits the never-sent proration once
// and the swap, payment, access and predecessor delete commit exactly once.
func TestUpgradeUnknownSuccessorResolvesOnlyFromExactReceipt(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.positiveProration()
	fx.newProduct.EntitlementsSpec = map[string]*int{"upgraded_access": nil}
	fx.gateway.createMode.Store("ambiguousLanded")
	_, err := fx.upgrade(t)
	require.ErrorIs(t, err, ErrCheckoutProcessing)
	require.Equal(t, intents.StatusUnknownNeedsVerify, fx.restartAndVerify(t).Status)

	_, err = fx.resolve(t, intents.Resolution{Step: "proration", ProviderReference: fx.gateway.saleTxn, Actor: "ops", Reason: "wrong step"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected, "an unsent step has nothing to resolve")
	_, err = fx.resolve(t, intents.Resolution{Step: "successor", ProviderReference: "rsub-missing", Actor: "ops", Reason: "wrong id"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)

	resolved, err := fx.resolve(t, intents.Resolution{Step: "successor", ProviderReference: fx.gateway.subID, Actor: "ops@example.test", Reason: "NMI subscription detail"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedRetryable, resolved.Status, "the verifier never submits the unsent proration")
	require.Zero(t, fx.gateway.saleCalls.Load())
	old, err := fx.svc.SubscriptionService.GetByID(fx.ctx, fx.existingSub.ID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, old.Status)
	var journal nmiUpgradeProgress
	require.NoError(t, json.Unmarshal(resolved.ResultEvidence, &journal))
	require.Equal(t, "ops@example.test", journal.Successor.Resolution["actor"])

	fx.newPrice.Amount = 999_000_000
	response, err := fx.upgrade(t)
	require.NoError(t, err)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Equal(t, "60.00", fx.gateway.saleAmount.Load(), "the frozen proration is submitted")
	next, err := fx.svc.SubscriptionService.GetByID(fx.ctx, *response.SubscriptionID)
	require.NoError(t, err)
	require.Equal(t, fx.gateway.subID, next.RailSubscriptionID)
	old, err = fx.svc.SubscriptionService.GetByID(fx.ctx, fx.existingSub.ID)
	require.NoError(t, err)
	require.Equal(t, models.StatusCancelled, old.Status)
	require.Equal(t, 1, fx.count(t, `SELECT count(*) FROM openrails.payments WHERE subscription_id=$1 AND amount=60000000`, next.ID))
	require.Equal(t, 1, fx.count(t, `SELECT count(*) FROM openrails.entitlements WHERE source_id=$1`, next.ID))
	require.Equal(t, 1, fx.count(t, `SELECT count(*) FROM openrails.rail_intents WHERE subscription_id=$1 AND intent_type=$2`, old.ID, intents.TypeNMIDeleteSubscription))
	require.Equal(t, 1, operatorResolutionLogs(t, fx.db, resolved.ID))

	_, err = fx.resolve(t, intents.Resolution{Step: "successor", ProviderReference: fx.gateway.subID, Actor: "ops", Reason: "again"})
	require.ErrorIs(t, err, intents.ErrResolutionNotUnknown)
	require.Equal(t, intents.StatusSucceeded, fx.restartAndVerify(t).Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
}

// A hidden proration sale converges from its exact transaction; provider-
// confirmed non-execution instead takes the definitive-refusal path, and is
// refused while the stable order reference shows a successful sale.
func TestUpgradeUnknownProrationResolution(t *testing.T) {
	t.Run("exact receipt", func(t *testing.T) {
		fx := newUpgradeAdoptFixture(t)
		fx.positiveProration()
		fx.gateway.saleMode.Store("ambiguousHidden")
		_, err := fx.upgrade(t)
		require.ErrorIs(t, err, ErrCheckoutProcessing)
		require.Equal(t, intents.StatusUnknownNeedsVerify, fx.restartAndVerify(t).Status)
		resolved, err := fx.resolve(t, intents.Resolution{Step: "proration", ProviderReference: fx.gateway.saleTxn, Actor: "ops", Reason: "NMI transaction detail"})
		require.NoError(t, err)
		require.Equal(t, intents.StatusSucceeded, resolved.Status)
		var p NMIUpgradePayload
		require.NoError(t, json.Unmarshal(resolved.Payload, &p))
		require.Equal(t, 1, fx.count(t, `SELECT count(*) FROM openrails.payments WHERE subscription_id=$1 AND transaction_id=$2`, p.NewSubscriptionID, fx.gateway.saleTxn))
		require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
		require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	})
	t.Run("non-execution", func(t *testing.T) {
		fx := newUpgradeAdoptFixture(t)
		fx.positiveProration()
		fx.gateway.saleMode.Store("ambiguousHidden")
		_, err := fx.upgrade(t)
		require.ErrorIs(t, err, ErrCheckoutProcessing)
		fx.gateway.saleVisible.Store(true)
		_, err = fx.resolve(t, intents.Resolution{Step: "proration", NotExecuted: true, Actor: "ops", Reason: "wrong"})
		require.ErrorIs(t, err, intents.ErrResolutionRejected)
		fx.gateway.saleVisible.Store(false)
		fx.gateway.saleLanded.Store(false)
		resolved, err := fx.resolve(t, intents.Resolution{Step: "proration", NotExecuted: true, Actor: "ops", Reason: "NMI confirmed no sale"})
		require.NoError(t, err)
		require.Equal(t, intents.StatusFailedTerminal, resolved.Status)
		var p NMIUpgradePayload
		require.NoError(t, json.Unmarshal(resolved.Payload, &p))
		next, err := fx.svc.SubscriptionService.GetByID(fx.ctx, p.NewSubscriptionID)
		require.NoError(t, err)
		require.Equal(t, models.StatusCancelled, next.Status)
		old, err := fx.svc.SubscriptionService.GetByID(fx.ctx, p.OldSubscriptionID)
		require.NoError(t, err)
		require.Equal(t, models.StatusActive, old.Status)
		require.Equal(t, 0, fx.count(t, `SELECT count(*) FROM openrails.payments WHERE subscription_id=$1`, p.NewSubscriptionID))
		_, err = fx.upgrade(t)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrCheckoutProcessing)
		require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	})
}

// Provider-confirmed non-execution of the successor terminates the operation
// with the predecessor intact and releases the predecessor for a new request.
func TestUpgradeSuccessorNonExecutionReleasesPredecessor(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.gateway.createMode.Store("ambiguousLost")
	_, err := fx.upgrade(t)
	require.ErrorIs(t, err, ErrCheckoutProcessing)
	resolved, err := fx.resolve(t, intents.Resolution{Step: "successor", NotExecuted: true, Actor: "ops", Reason: "NMI confirmed no subscription"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedTerminal, resolved.Status)
	old, err := fx.svc.SubscriptionService.GetByID(fx.ctx, fx.existingSub.ID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, old.Status)

	fx.gateway.createMode.Store("approve")
	fx.req.IdempotencyKey = uuid.NewString()
	response, err := fx.upgrade(t)
	require.NoError(t, err)
	require.Equal(t, "success", response.Status)
	require.EqualValues(t, 2, fx.gateway.createCalls.Load(), "only a definitively unexecuted enrollment permits a new operation")
}
