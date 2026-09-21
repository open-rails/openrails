//go:build integration

package intents

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// A command starts before another transaction accepts a new quote/card/period.
// Its blocked write must not replay the earlier full-row image after that
// transaction commits. This covers both narrow writes and locked partial edits.
func TestPartialSubscriptionCommandsPreserveNewlyAcceptedBillingTerms(t *testing.T) {
	for _, command := range []string{"notes", "status", "extend", "cancel", "delete_completed"} {
		t.Run(command, func(t *testing.T) {
			fx := seedPastDueSubscriptionForMerchant(t, uuid.New())
			ctx := fx.handlerCtx()
			_, err := fx.db.Pool().Exec(ctx, `UPDATE billing.subscriptions SET status='active',deletion_scheduled_at=now() WHERE id=$1`, fx.subID)
			require.NoError(t, err)
			prices := catalog.NewPriceService(fx.db)
			products := catalog.NewProductService(fx.db)
			admin := &subscriptions.AdminSubscriptionService{SubscriptionService: subscriptions.NewSubscriptionService(fx.db, prices, products, nil), NotificationService: subscriptions.NewNotificationService(fx.db, nil)}
			admin.SetDeferredDeleteScheduler(NewNMIDeleteScheduler(fx.db, nil, OriginAdmin, "admin cancellation"))
			target, currentPrice, method := uuid.New(), uuid.New(), uuid.New()
			for index, id := range []uuid.UUID{target, currentPrice} {
				_, err = fx.db.Pool().Exec(ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew,key) VALUES($1,$2,$3,$5,'USD',720,true,$4)`, id, fx.merchantID, fx.payload.Renewal.ProductID, "partial-"+id.String(), int64(8000000+index*1000000))
				require.NoError(t, err)
			}
			_, err = fx.db.Pool().Exec(ctx, `INSERT INTO billing.payment_methods(id,merchant_id,customer_id,rail,psp_id,rail_customer_ref,rail_method_ref,initial_transaction_id,stored_credential_recurring_ref,rebill_driver) SELECT $1,merchant_id,customer_id,rail,psp_id,rail_customer_ref,'new-billing-profile',initial_transaction_id,stored_credential_recurring_ref,rebill_driver FROM billing.payment_methods WHERE id=$2`, method, fx.payload.PaymentMethodID)
			require.NoError(t, err)
			tx, err := fx.db.Pool().Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
			d := fx.db.NewWithPgxTx(tx)
			_, err = subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, fx.subID)
			require.NoError(t, err)
			var pid int
			require.NoError(t, tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid))
			deletePayload, err := json.Marshal(NMIDeletePayload{UserID: fx.payload.Renewal.CustomerID.String(), RailSubscriptionID: fx.payload.RailSubscriptionID})
			require.NoError(t, err)
			result := make(chan error, 1)
			go func() {
				switch command {
				case "notes":
					result <- admin.UpdateSubscription(ctx, fx.subID, map[string]any{"notes": "operator note"})
				case "status":
					result <- admin.UpdateSubscription(ctx, fx.subID, map[string]any{"status": models.StatusUnknown})
				case "extend":
					result <- admin.ExtendSubscriptionByDuration(ctx, fx.subID, 24*time.Hour)
				case "cancel":
					result <- admin.CancelSubscription(ctx, fx.subID, "requested", false)
				case "delete_completed":
					result <- NewNMIDeleteHandler(fx.db, fullModeConfig(), nil, nil).finalize(ctx, gen.OpenrailsRailIntent{MerchantID: fx.merchantID, SubscriptionID: &fx.subID, PspID: &fx.pspID, Payload: deletePayload})
				}
			}()
			require.Eventually(t, func() bool {
				var waiting bool
				err := fx.db.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting)
				return err == nil && waiting
			}, 10*time.Second, 10*time.Millisecond)
			end := fx.periodEnd.Add(-time.Hour)
			start := end.Add(-30 * 24 * time.Hour)
			_, err = tx.Exec(ctx, `UPDATE billing.subscriptions SET status='past_due',price_id=$2,scheduled_price_id=$3,payment_method_id=$4,current_period_starts_at=$5,current_period_ends_at=$6,gateway_response='{"provider_update":"retained"}' WHERE id=$1`, fx.subID, currentPrice, target, method, start, end)
			require.NoError(t, err)
			accepted, err := NewManualRebillHandler(d, fullModeConfig(), nil, nil).EnqueueScheduled(ctx, fx.subID)
			require.NoError(t, err)
			require.NoError(t, tx.Commit(ctx))
			err = <-result
			if command == "extend" || command == "cancel" {
				require.ErrorIs(t, err, subscriptions.ErrSubscriptionNotActive)
			} else {
				require.NoError(t, err)
			}
			current, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(ctx, fx.subID)
			require.NoError(t, err)
			require.Equal(t, currentPrice, current.PriceID)
			require.Equal(t, &target, current.ScheduledPriceID)
			require.Equal(t, &method, current.PaymentMethodID)
			require.True(t, current.CurrentPeriodStartsAt.Equal(start))
			require.True(t, current.CurrentPeriodEndsAt.Equal(end))
			expectedStatus := models.StatusPastDue
			if command == "status" {
				expectedStatus = models.StatusUnknown
			}
			require.Equal(t, expectedStatus, current.Status)
			var metadata map[string]any
			require.NoError(t, json.Unmarshal(current.Metadata, &metadata))
			require.Equal(t, "retained", metadata["provider_update"])
			if command == "notes" {
				require.Equal(t, "operator note", metadata["admin_notes"])
			}
			if command == "delete_completed" {
				require.Nil(t, current.DeletionScheduledAt)
			} else {
				require.NotNil(t, current.DeletionScheduledAt)
			}
			retained, err := NewStore(fx.db).Get(ctx, accepted.ID)
			require.NoError(t, err)
			require.JSONEq(t, string(accepted.Payload), string(retained.Payload))
			require.Equal(t, StatusPending, retained.Status)
		})
	}
}

func TestAdminExtensionAndCancellationKeepFreshBillingFields(t *testing.T) {
	fx := seedPastDueSubscriptionForMerchant(t, uuid.New())
	ctx := fx.handlerCtx()
	_, client := newFakeNMIRebillGateway(t, fx)
	h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
	accepted, err := h.EnqueueScheduled(ctx, fx.subID)
	require.NoError(t, err)
	done, err := fx.rebillRunner(client, fullModeConfig()).ExecuteByID(ctx, accepted.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, done.Status)
	prices, products := catalog.NewPriceService(fx.db), catalog.NewProductService(fx.db)
	admin := &subscriptions.AdminSubscriptionService{SubscriptionService: subscriptions.NewSubscriptionService(fx.db, prices, products, nil), NotificationService: subscriptions.NewNotificationService(fx.db, nil), EntitlementService: entitlements.NewEntitlementService(fx.db)}
	admin.SetDeferredDeleteScheduler(NewNMIDeleteScheduler(fx.db, nil, OriginAdmin, "admin cancellation"))
	before, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(ctx, fx.subID)
	require.NoError(t, err)
	require.NoError(t, admin.ExtendSubscriptionByDuration(ctx, fx.subID, 24*time.Hour))
	extended, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(ctx, fx.subID)
	require.NoError(t, err)
	require.True(t, extended.CurrentPeriodEndsAt.Equal(before.CurrentPeriodEndsAt.Add(24*time.Hour)))
	var entitlementEnd *time.Time
	require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT end_at FROM billing.entitlements WHERE source_id=$1 AND revoked_at IS NULL LIMIT 1`, fx.subID).Scan(&entitlementEnd))
	require.Nil(t, entitlementEnd, "active auto-renewing access remains continuous when the paid period extends")
	target := uuid.New()
	_, err = fx.db.Pool().Exec(ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew,key) VALUES($1,$2,$3,8000000,'USD',720,true,$4)`, target, fx.merchantID, fx.payload.Renewal.ProductID, "cancel-quote-"+target.String())
	require.NoError(t, err)
	// The pending quote is deliberately preserved by a lifecycle cancellation;
	// cancellation owns status/retry fields and a separate durable provider delete.
	_, err = fx.db.Pool().Exec(ctx, `UPDATE billing.subscriptions SET scheduled_price_id=$2 WHERE id=$1`, fx.subID, target)
	require.NoError(t, err)
	require.NoError(t, admin.CancelSubscription(ctx, fx.subID, "requested", false))
	cancelled, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(ctx, fx.subID)
	require.NoError(t, err)
	require.Equal(t, models.StatusCancelled, cancelled.Status)
	require.Equal(t, &target, cancelled.ScheduledPriceID)
	require.Equal(t, extended.PriceID, cancelled.PriceID)
	require.Equal(t, extended.PaymentMethodID, cancelled.PaymentMethodID)
	require.True(t, cancelled.CurrentPeriodEndsAt.Equal(*extended.CurrentPeriodEndsAt))
	require.NotNil(t, cancelled.DeletionScheduledAt)
	var deletes int
	require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='nmi_delete_subscription' AND origin='admin'`, fx.subID).Scan(&deletes))
	require.Equal(t, 1, deletes)
}

func TestDeleteCompletionOnlyClearsItsAcceptedProviderTarget(t *testing.T) {
	fx := seedPastDueSubscriptionForMerchant(t, uuid.New())
	ctx := fx.handlerCtx()
	payload, err := json.Marshal(NMIDeletePayload{UserID: fx.payload.Renewal.CustomerID.String(), RailSubscriptionID: fx.payload.RailSubscriptionID})
	require.NoError(t, err)
	completed := gen.OpenrailsRailIntent{MerchantID: fx.merchantID, SubscriptionID: &fx.subID, PspID: &fx.pspID, Payload: payload}
	other := dbtest.EnsureTestPSP(ctx, t, fx.db.Pool(), fx.merchantID, "other-nmi-"+uuid.NewString())
	h := NewNMIDeleteHandler(fx.db, fullModeConfig(), nil, nil)
	for _, target := range []struct {
		name      string
		psp       uuid.UUID
		reference string
		clear     bool
	}{
		{"different account same reference", other, fx.payload.RailSubscriptionID, false},
		{"same account different reference", fx.pspID, "replacement-reference", false},
		{"same accepted target", fx.pspID, fx.payload.RailSubscriptionID, true},
	} {
		t.Run(target.name, func(t *testing.T) {
			_, err := fx.db.Pool().Exec(ctx, `UPDATE billing.subscriptions SET psp_id=$2,rail_subscription_id=$3,deletion_scheduled_at=now() WHERE id=$1`, fx.subID, target.psp, target.reference)
			require.NoError(t, err)
			for range 2 {
				require.NoError(t, h.finalize(ctx, completed))
			}
			current, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(ctx, fx.subID)
			require.NoError(t, err)
			if target.clear {
				require.Nil(t, current.DeletionScheduledAt)
			} else {
				require.NotNil(t, current.DeletionScheduledAt)
			}
		})
	}
	_, err = fx.db.Pool().Exec(ctx, `UPDATE billing.subscriptions SET deletion_scheduled_at=now() WHERE id=$1`, fx.subID)
	require.NoError(t, err)
	completed.Payload = []byte(`{}`)
	require.Error(t, h.finalize(ctx, completed))
	current, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(ctx, fx.subID)
	require.NoError(t, err)
	require.NotNil(t, current.DeletionScheduledAt)
}

func TestDeleteProducerSeparatesCompletedProviderTargets(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	scheduler := NewNMIDeleteScheduler(fx.db, nil, OriginAdmin, "cancel exact target")
	require.NoError(t, scheduler.ScheduleNMIDelete(ctx, fx.userID.String(), fx.subID, time.Now()))
	first, err := fx.store.GetByIdempotencyKey(ctx, NMIDeleteIdempotencyKey(fx.subID, fx.pspID, fx.psid))
	require.NoError(t, err)
	a, clientA := newFakeNMI(t, fx.psid, true)
	done, err := fx.runner(clientA, fullModeConfig()).ExecuteByID(ctx, first.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, done.Status)
	require.EqualValues(t, 1, a.deleteCalls.Load())
	other := dbtest.EnsureTestPSP(ctx, t, fx.db.Pool(), dbtest.TestMerchantID.UUID(), "replacement-mobius-"+uuid.NewString())
	reference := fx.psid // Independent accounts may reuse the identical provider id.
	_, err = fx.db.Pool().Exec(ctx, `UPDATE billing.subscriptions SET psp_id=$2,rail_subscription_id=$3,deletion_scheduled_at=now() WHERE id=$1`, fx.subID, other, reference)
	require.NoError(t, err)
	require.NoError(t, scheduler.ScheduleNMIDelete(ctx, fx.userID.String(), fx.subID, time.Now()))
	second, err := fx.store.GetByIdempotencyKey(ctx, NMIDeleteIdempotencyKey(fx.subID, other, reference))
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
	b, clientB := newFakeNMI(t, reference, true)
	done, err = fx.runner(clientB, fullModeConfig()).ExecuteByID(ctx, second.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, done.Status)
	require.NoError(t, scheduler.ScheduleNMIDelete(ctx, fx.userID.String(), fx.subID, time.Now()))
	replay, err := fx.store.GetByIdempotencyKey(ctx, NMIDeleteIdempotencyKey(fx.subID, other, reference))
	require.NoError(t, err)
	require.Equal(t, second.ID, replay.ID)
	require.Equal(t, StatusSucceeded, replay.Status)
	_, err = fx.runner(clientB, fullModeConfig()).ExecuteByID(ctx, replay.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, a.deleteCalls.Load())
	require.EqualValues(t, 1, b.deleteCalls.Load())
}

func TestOldDeleteCannotTargetOrUndoAReplacementBinding(t *testing.T) {
	fx := seedCancelledNMISubscription(t, time.Now().Add(-time.Minute))
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	scheduler := NewNMIDeleteScheduler(fx.db, nil, OriginAdmin, "cancel exact target")
	require.NoError(t, scheduler.ScheduleNMIDelete(ctx, fx.userID.String(), fx.subID, time.Now()))
	first, err := fx.store.GetByIdempotencyKey(ctx, NMIDeleteIdempotencyKey(fx.subID, fx.pspID, fx.psid))
	require.NoError(t, err)
	reference := "replacement-" + uuid.NewString()
	_, err = fx.db.Pool().Exec(ctx, `UPDATE billing.subscriptions SET rail_subscription_id=$2 WHERE id=$1`, fx.subID, reference)
	require.NoError(t, err)
	gateway, client := newFakeNMI(t, reference, true)
	h := NewNMIDeleteHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
	executed, verified := h.Execute(ctx, first), h.Verify(ctx, first)
	require.Zero(t, gateway.deleteCalls.Load(), "old A must never delete current B")
	require.Zero(t, gateway.queryCalls.Load())
	require.Equal(t, OutcomeParked, executed.Class)
	require.Equal(t, OutcomeAmbiguous, verified.Class)
	require.NoError(t, scheduler.ScheduleNMIDelete(ctx, fx.userID.String(), fx.subID, time.Now()))
	second, err := fx.store.GetByIdempotencyKey(ctx, NMIDeleteIdempotencyKey(fx.subID, fx.pspID, reference))
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID, "a replacement reference on the same account has its own command")
	require.NoError(t, scheduler.CancelNMIDelete(ctx, fx.userID.String(), fx.subID))
	current, err := fx.store.Get(ctx, second.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSuperseded, current.Status)
	historical, err := fx.store.Get(ctx, first.ID)
	require.NoError(t, err)
	require.Equal(t, StatusPending, historical.Status, "undo of B must preserve unresolved historical A")
}
