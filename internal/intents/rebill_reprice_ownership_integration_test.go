//go:build integration

package intents

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedOwnedReprice(t *testing.T) (rebillFixture, uuid.UUID, uuid.UUID) {
	t.Helper()
	fx := seedPastDueSubscription(t)
	target := uuid.New()
	_, err := fx.db.Pool().Exec(fx.handlerCtx(), `INSERT INTO openrails.prices(id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew,key) VALUES($1,$2,$3,8000000,'USD',720,true,$4)`, target, fx.merchantID, fx.payload.Renewal.ProductID, "owned-"+target.String())
	require.NoError(t, err)
	change, err := subscriptions.NewRepriceRepo(fx.db).CreateSubscriptionReprice(fx.handlerCtx(), fx.subID, fx.payload.Renewal.PriceID, target, time.Now().Add(-time.Hour), nil, false)
	require.NoError(t, err)
	return fx, target, change.ID
}

func TestPreparedRepriceRemainsOwnedAfterLostResponseAndDecline(t *testing.T) {
	for _, decline := range []bool{false, true} {
		t.Run(fmt.Sprintf("decline_%t", decline), func(t *testing.T) {
			fx, target, change := seedOwnedReprice(t)
			ctx := fx.handlerCtx()
			gateway, client := newFakeNMIRebillGateway(t, fx)
			gateway.loseUpdateResponse.Store(true)
			h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
			accepted, err := h.EnqueueScheduled(ctx, fx.subID)
			require.NoError(t, err)
			gateway.beforeUpdate = func() {
				// The HTTP callback reads on a different pooled connection, so a write
				// still hidden in an uncommitted preparation transaction cannot pass.
				var raw []byte
				err := fx.db.Pool().QueryRow(ctx, `SELECT result_evidence FROM openrails.rail_intents WHERE id=$1`, accepted.ID).Scan(&raw)
				if assert.NoError(t, err) {
					observed := accepted
					observed.ResultEvidence = raw
					_, found, err := loadRebillPreparation(observed)
					assert.NoError(t, err)
					assert.True(t, found)
				}
			}
			runner := fx.rebillRunner(client, fullModeConfig())
			row, err := runner.ExecuteByID(ctx, accepted.ID)
			require.NoError(t, err)
			require.Equal(t, StatusPending, row.Status)
			require.Zero(t, gateway.saleCalls.Load())
			require.Equal(t, "8.00", gateway.amount.Load())
			repo := subscriptions.NewRepriceRepo(fx.db)
			require.ErrorIs(t, repo.Cancel(ctx, change), subscriptions.ErrRebillTermsCommitted)
			require.ErrorIs(t, repo.BlockScheduledReprice(ctx, change, "push failed"), subscriptions.ErrRebillTermsCommitted)
			if decline {
				gateway.saleBody.Store("response=2&response_code=202&responsetext=Insufficient+funds")
				row, err = runner.ExecuteByID(ctx, accepted.ID)
				require.NoError(t, err)
				require.Equal(t, StatusFailedTerminal, row.Status)
				require.ErrorIs(t, repo.Cancel(ctx, change), subscriptions.ErrRebillTermsCommitted)
			}
			_, err = repo.CreateSubscriptionReprice(ctx, fx.subID, fx.payload.Renewal.PriceID, target, time.Now(), nil, false)
			require.ErrorIs(t, err, subscriptions.ErrRebillTermsCommitted)
			_, err = subscriptions.NewSubscriptionRepo(fx.db).SchedulePriceChange(ctx, fx.subID, fx.payload.Renewal.PriceID, target)
			require.ErrorIs(t, err, subscriptions.ErrRebillTermsCommitted)
			_, err = fx.store.Enqueue(ctx, EnqueueParams{MerchantID: fx.merchantID, Provider: "nmi", PspID: fx.pspID, IntentType: "nmi_upgrade", SubscriptionID: &fx.subID, PriceID: &target, IdempotencyKey: "blocked-upgrade-" + uuid.NewString(), NextAttemptAt: time.Now(), Origin: OriginUser})
			require.ErrorIs(t, err, subscriptions.ErrRebillTermsCommitted)
			if decline {
				// A previously blocked accepted quote must not become replaceable merely
				// because the charge declined. This is a restored/imported state fixture;
				// the live Block path above already refuses this transition.
				_, err = fx.db.Pool().Exec(ctx, `UPDATE openrails.subscription_reprices SET status='blocked',blocked_reason='rail_push_failed: restored pending quote' WHERE id=$1`, change)
				require.NoError(t, err)
				require.ErrorIs(t, repo.Unblock(ctx, change), subscriptions.ErrRebillTermsCommitted)
				return
			}
			row, err = runner.ExecuteByID(ctx, accepted.ID)
			require.NoError(t, err)
			require.Equal(t, StatusSucceeded, row.Status)
			require.Equal(t, target, *fx.subscription(t).PriceID)
			require.EqualValues(t, 1, gateway.updateCalls.Load())
			require.EqualValues(t, 1, gateway.saleCalls.Load())
			// The applied historical quote no longer owns unrelated future changes.
			_, err = repo.CreateSubscriptionReprice(ctx, fx.subID, target, fx.payload.Renewal.PriceID, time.Now().Add(31*24*time.Hour), nil, false)
			require.NoError(t, err)
		})
	}
}

func TestNeverPreparedTerminalRebillReleasesReprice(t *testing.T) {
	fx, target, change := seedOwnedReprice(t)
	ctx := fx.handlerCtx()
	gateway, client := newFakeNMIRebillGateway(t, fx)
	h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
	accepted, err := h.EnqueueScheduled(ctx, fx.subID)
	require.NoError(t, err)
	require.ErrorIs(t, subscriptions.NewRepriceRepo(fx.db).Cancel(ctx, change), subscriptions.ErrRebillTermsCommitted, "absence of evidence does not release unresolved work")
	_, err = fx.db.Pool().Exec(ctx, `UPDATE openrails.payment_methods SET rail_method_ref='replacement' WHERE id=$1`, fx.payload.PaymentMethodID)
	require.NoError(t, err)
	row, err := fx.rebillRunner(client, fullModeConfig()).ExecuteByID(ctx, accepted.ID)
	require.NoError(t, err)
	require.Equal(t, StatusFailedTerminal, row.Status)
	require.Contains(t, string(row.ResultEvidence), `"not_executed": true`)
	require.Zero(t, gateway.updateCalls.Load())
	require.Zero(t, gateway.saleCalls.Load())
	repo := subscriptions.NewRepriceRepo(fx.db)
	require.NoError(t, repo.Cancel(ctx, change))
	_, err = repo.CreateSubscriptionReprice(ctx, fx.subID, fx.payload.Renewal.PriceID, target, time.Now(), nil, false)
	require.NoError(t, err)
}

func TestRepriceCancelAndRebillAdmissionSerializeBothOrders(t *testing.T) {
	for _, first := range []string{"cancel", "admission"} {
		t.Run(first, func(t *testing.T) {
			fx, target, change := seedOwnedReprice(t)
			ctx := fx.handlerCtx()
			_, client := newFakeNMIRebillGateway(t, fx)
			tx, err := fx.db.Pool().Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
			d := fx.db.NewWithPgxTx(tx)
			_, err = subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, fx.subID)
			require.NoError(t, err)
			var pid int
			require.NoError(t, tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid))
			result := make(chan error, 1)
			var accepted gen.OpenrailsRailIntent
			if first == "cancel" {
				require.NoError(t, subscriptions.NewRepriceRepo(d).Cancel(ctx, change))
				go func() {
					var err error
					accepted, err = NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil).EnqueueScheduled(ctx, fx.subID)
					result <- err
				}()
			} else {
				accepted, err = NewManualRebillHandler(d, fullModeConfig(), fakeNMIResolver{client: client}, nil).EnqueueScheduled(ctx, fx.subID)
				require.NoError(t, err)
				go func() { result <- subscriptions.NewRepriceRepo(fx.db).Cancel(ctx, change) }()
			}
			require.Eventually(t, func() bool {
				var waiting bool
				err := fx.db.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting)
				return err == nil && waiting
			}, 10*time.Second, 10*time.Millisecond)
			require.NoError(t, tx.Commit(ctx))
			err = <-result
			if first == "admission" {
				require.ErrorIs(t, err, subscriptions.ErrRebillTermsCommitted)
			} else {
				require.NoError(t, err)
			}
			p, err := subscriptions.DecodeManualRebillPayload(accepted)
			require.NoError(t, err)
			if first == "admission" {
				require.Equal(t, target, p.Renewal.PriceID)
			} else {
				require.Equal(t, fx.payload.Renewal.PriceID, p.Renewal.PriceID)
				require.Nil(t, p.Renewal.RepriceID)
			}
		})
	}
}

func TestHistoricalScheduledTargetDoesNotOwnANewerQuote(t *testing.T) {
	clock := clockwork.NewFakeClockAt(time.Now().Add(-90 * 24 * time.Hour).UTC().Truncate(time.Second))
	fx := seedPastDueSubscriptionAt(t, uuid.New(), clock.Now())
	ctx := fx.handlerCtx()
	target := uuid.New()
	_, err := fx.db.Pool().Exec(ctx, `INSERT INTO openrails.prices(id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew,key) VALUES($1,$2,$3,8000000,'USD',720,true,$4)`, target, fx.merchantID, fx.payload.Renewal.ProductID, "reused-"+target.String())
	require.NoError(t, err)
	repo := subscriptions.NewSubscriptionRepo(fx.db)
	_, err = repo.SchedulePriceChange(ctx, fx.subID, fx.payload.Renewal.PriceID, target)
	require.NoError(t, err)
	gateway, client := newFakeNMIRebillGateway(t, fx)
	h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, clock)
	accepted, err := h.EnqueueScheduled(ctx, fx.subID)
	require.NoError(t, err)
	runner := &Runner{Store: fx.store, Config: fullModeConfig(), Registry: NewRegistry(h), Clock: clock}
	done, err := runner.ExecuteByID(ctx, accepted.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, done.Status)
	current, err := repo.GetByID(ctx, fx.subID)
	require.NoError(t, err)
	require.Nil(t, current.ScheduledPriceID)
	// A later observed renewal applies an explicit reprice back to A. The old
	// A->B receipt remains valid history, but its accepted period has ended.
	clock.Advance(current.CurrentPeriodEndsAt.Add(time.Minute).Sub(clock.Now()))
	_, err = subscriptions.NewRepriceRepo(fx.db).CreateSubscriptionReprice(ctx, fx.subID, target, fx.payload.Renewal.PriceID, clock.Now().Add(-time.Minute), nil, false)
	require.NoError(t, err)
	start, end := *current.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt.Add(30*24*time.Hour)
	require.NoError(t, h.lifecycle(fx.db).RenewMembership(ctx, &subscriptions.RenewMembershipParams{Rail: "nmi", RailSubscriptionID: fx.payload.RailSubscriptionID, TransactionID: "return-to-A-" + uuid.NewString(), Amount: fx.payload.Renewal.Amount, AmountProvided: true, Currency: "USD", CurrentPeriodStartsAt: &start, CurrentPeriodEndsAt: &end}))
	_, err = repo.SchedulePriceChange(ctx, fx.subID, fx.payload.Renewal.PriceID, target)
	require.NoError(t, err)

	// The actual tier-change admission path must likewise ignore the old
	// completed quote. This tests admission only; it sends no second upgrade.
	_, err = fx.store.Enqueue(ctx, EnqueueParams{MerchantID: fx.merchantID, Provider: "nmi", PspID: fx.pspID, IntentType: "nmi_upgrade", SubscriptionID: &fx.subID, PriceID: &target, IdempotencyKey: "later-upgrade-" + uuid.NewString(), NextAttemptAt: clock.Now(), Origin: OriginUser})
	require.NoError(t, err)
	require.EqualValues(t, 1, gateway.saleCalls.Load())
}

func TestRebillAdmissionRefusesAnAcceptedNMIUpgrade(t *testing.T) {
	fx := seedPastDueSubscription(t)
	ctx := fx.handlerCtx()
	_, client := newFakeNMIRebillGateway(t, fx)
	_, err := fx.store.Enqueue(ctx, EnqueueParams{MerchantID: fx.merchantID, Provider: "nmi", PspID: fx.pspID, IntentType: "nmi_upgrade", SubscriptionID: &fx.subID, PriceID: &fx.payload.Renewal.PriceID, IdempotencyKey: "first-upgrade-" + uuid.NewString(), NextAttemptAt: time.Now(), Origin: OriginUser})
	require.NoError(t, err)
	_, err = NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil).EnqueueScheduled(ctx, fx.subID)
	require.ErrorIs(t, err, subscriptions.ErrRebillTermsCommitted)
	var count int
	require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.rail_intents WHERE subscription_id=$1 AND intent_type='manual_rebill'`, fx.subID).Scan(&count))
	require.Zero(t, count)
}
