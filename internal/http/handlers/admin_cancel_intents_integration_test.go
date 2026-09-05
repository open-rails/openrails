//go:build integration

package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// or#896 tasks 3 + 4: the MERCHANT-initiated cancel (admin API) drives the same
// durable provider intents as the user-initiated one. It used to call NMI
// synchronously with no intent and no verify leg — an unresolvable PSP logged a
// warning and returned nil, so the local row went terminal while NMI kept
// rebilling — and it REFUSED CCBill outright while the findings queue happily
// cancelled it.

// adminSubscriptionService builds the production AdminSubscriptionService with
// admin-origin intent schedulers, exactly as build_runtime wires it.
func (fx *findingsFixture) adminSubscriptionService() *subscriptions.AdminSubscriptionService {
	fx.t.Helper()
	clock := clockwork.NewRealClock()
	priceSvc := catalog.NewPriceService(fx.dbi)
	productSvc := catalog.NewProductService(fx.dbi)
	svc := subscriptions.NewAdminSubscriptionService(
		subscriptions.NewSubscriptionService(fx.dbi, priceSvc, productSvc, nil, clock),
		productSvc,
		priceSvc,
		entitlements.NewEntitlementService(fx.dbi, clock),
		subscriptions.NewNotificationService(fx.dbi, nil),
		payments.NewPaymentService(fx.dbi, clock),
		clock,
	)
	svc.SetDeferredDeleteScheduler(intents.NewNMIDeleteScheduler(fx.dbi, nil, intents.OriginAdmin, "merchant cancel"))
	svc.SetCCBillCancelScheduler(intents.NewCCBillCancelScheduler(fx.dbi, nil, intents.OriginAdmin, "merchant cancel"))
	return svc
}

func (fx *findingsFixture) subscriptionRow(id uuid.UUID) *models.Subscription {
	fx.t.Helper()
	sub, err := subscriptions.NewSubscriptionRepo(fx.dbi).GetByID(fx.ctx, id)
	require.NoError(fx.t, err)
	return sub
}

type railIntentRow struct {
	ID     uuid.UUID
	Status string
	Origin string
}

func (fx *findingsFixture) railIntent(t *testing.T, intentType string, subID uuid.UUID) railIntentRow {
	t.Helper()
	var row railIntentRow
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT id, status, origin FROM openrails.rail_intents
		 WHERE merchant_id = $1 AND intent_type = $2 AND subscription_id = $3`,
		fx.merchant, intentType, subID).Scan(&row.ID, &row.Status, &row.Origin))
	return row
}

// TestMerchantCancelNMIRidesDurableIntent: merchant cancel -> local cancel +
// admin-origin delete intent committed together, the deferred-delete marker
// pinning "rail side not yet confirmed" -> the executor confirms against the
// (fake) gateway -> intent succeeded and the marker clears, i.e. the local row
// only becomes terminal once NMI actually dropped the schedule.
func TestMerchantCancelNMIRidesDurableIntent(t *testing.T) {
	fx := newFindingsFixture(t)
	subID := fx.seedActiveSubscription("psid-merchant-cancel")

	require.NoError(t, fx.adminSubscriptionService().CancelSubscription(fx.ctx, subID, "chargeback risk", true))

	sub := fx.subscriptionRow(subID)
	assert.Equal(t, models.StatusCancelled, sub.Status)
	require.NotNil(t, sub.CancelType)
	assert.Equal(t, models.CancelTypeMerchant, *sub.CancelType)
	require.NotNil(t, sub.DeletionScheduledAt,
		"the rail-side delete is still unconfirmed: the marker must hold until the intent says otherwise")

	intent := fx.railIntent(t, intents.TypeNMIDeleteSubscription, subID)
	assert.Equal(t, intents.StatusPending, intent.Status, "the remote cancel is durable, not fire-and-forget")
	assert.Equal(t, string(intents.OriginAdmin), intent.Origin)

	// Drain: verify-then-execute against the gateway, then finalize.
	_, err := fx.deleteRunner().RunExecuteOnce(fx.ctx)
	require.NoError(t, err)

	assert.EqualValues(t, 1, fx.fake.deleteCalls.Load(), "the schedule was actually deleted at NMI")
	after := fx.railIntent(t, intents.TypeNMIDeleteSubscription, subID)
	assert.Equal(t, intents.StatusSucceeded, after.Status)
	assert.Nil(t, fx.subscriptionRow(subID).DeletionScheduledAt,
		"confirmation clears the marker — only now is the cancellation terminal")
}

// TestMerchantCancelNMIAmbiguousOutcomeParksForVerification: the gateway
// answers the delete with a transport failure. The intent must park as
// unknown_needs_verify (never blind-complete), and the local row must keep its
// unconfirmed marker rather than claim the schedule is gone.
func TestMerchantCancelNMIAmbiguousOutcomeParksForVerification(t *testing.T) {
	fx := newFindingsFixture(t)
	subID := fx.seedActiveSubscription("psid-merchant-ambiguous")
	fx.fake.deleteFails.Store(true)

	require.NoError(t, fx.adminSubscriptionService().CancelSubscription(fx.ctx, subID, "fraud", true))

	_, err := fx.deleteRunner().RunExecuteOnce(fx.ctx)
	require.NoError(t, err)

	intent := fx.railIntent(t, intents.TypeNMIDeleteSubscription, subID)
	assert.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status,
		"a possibly-sent delete verifies; it is never reported done on a guess")
	require.NotNil(t, fx.subscriptionRow(subID).DeletionScheduledAt,
		"the marker survives an unknown outcome — the row must not lie about the rail")

	// The delete did land; the verifier's read resolves it and finalizes.
	fx.fake.deleteFails.Store(false)
	fx.fake.subDeleted.Store(true)
	_, err = fx.dbi.Pool().Exec(fx.ctx,
		`UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1`, intent.ID)
	require.NoError(t, err)
	_, err = fx.deleteRunner().RunVerifyOnce(fx.ctx)
	require.NoError(t, err)

	assert.Equal(t, intents.StatusSucceeded, fx.railIntent(t, intents.TypeNMIDeleteSubscription, subID).Status)
	assert.Nil(t, fx.subscriptionRow(subID).DeletionScheduledAt)
}

// TestMerchantCancelCCBillDrivesTheLiveVerifiedCancel: the admin API used to
// REFUSE CCBill ("cancel operation not supported for rail 'ccbill'") while the
// findings queue cancelled it happily — one operation, two answers. Both
// surfaces now queue the same admin-origin ccbill_cancel_subscription intent,
// and it drains through the live-verified DataLink SMS cancel (#696 Phase 0).
func TestMerchantCancelCCBillDrivesTheLiveVerifiedCancel(t *testing.T) {
	fx := newFindingsFixture(t)
	subID := fx.seedActiveCCBillSubscription("ccsub-admin-" + uuid.NewString()[:8])

	require.NoError(t, fx.adminSubscriptionService().CancelSubscription(fx.ctx, subID, "merchant request", true))

	sub := fx.subscriptionRow(subID)
	assert.Equal(t, models.StatusCancelled, sub.Status)
	require.NotNil(t, sub.CancelType)
	assert.Equal(t, models.CancelTypeMerchant, *sub.CancelType)

	intent := fx.railIntent(t, intents.TypeCCBillCancelSubscription, subID)
	assert.Equal(t, intents.StatusPending, intent.Status, "the remote cancel is durable, not refused")
	assert.Equal(t, string(intents.OriginAdmin), intent.Origin,
		"same intent, same origin as the findings-queue action (TestFindingsQueueApproveCCBillCancelOnly)")

	// It drains through the same DataLink choke point the findings queue uses.
	fake, client := newFakeCCBillSMS(t)
	runner := &intents.Runner{
		Store:    intents.NewStore(fx.dbi),
		Registry: intents.NewRegistry(newAdminFindingsCCBillCancelHandler(fx.dbi, client)),
		Config:   fx.rt.Config,
		Breaker:  intents.NewVolumeBreaker(fx.dbi),
	}
	_, err := runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	assert.Equal(t, intents.StatusSucceeded, fx.railIntent(t, intents.TypeCCBillCancelSubscription, subID).Status)
	assert.EqualValues(t, 1, fake.cancelCalls.Load(), "the merchant cancel actually reached CCBill")
}

// TestMerchantCancelSolanaNamesTheWalletEndpoints: a Solana cancel needs the
// subscriber's signature; the merchant surface says so instead of half-doing it.
func TestMerchantCancelSolanaNamesTheWalletEndpoints(t *testing.T) {
	fx := newFindingsFixture(t)
	productID, priceID := fx.seedSecondProduct()
	subID := uuid.New()
	now := time.Now().UTC()
	fx.exec(`INSERT INTO openrails.subscriptions
	          (id, price_id, product_id, status, rail, rail_subscription_id,
	           current_period_starts_at, current_period_ends_at, started_at, customer_id, merchant_id, psp_id)
	        VALUES ($1, $2, $3, 'active', 'solana', 'pda-merchant-cancel', $4, $5, $4, $6, $7, $8)`,
		subID, priceID, productID, now.Add(-24*time.Hour), now.Add(6*24*time.Hour), fx.customer, fx.merchant, fx.pspFor("solana"))

	err := fx.adminSubscriptionService().CancelSubscription(fx.ctx, subID, "merchant request", true)
	require.ErrorIs(t, err, subscriptions.ErrSolanaCancelNeedsWalletSignature)
	assert.Contains(t, err.Error(), "solana-cancel-tx")
	assert.Contains(t, err.Error(), "solana-cancel")

	assert.Equal(t, models.StatusActive, fx.subscriptionRow(subID).Status,
		"a refused cancel changes nothing locally")
}
