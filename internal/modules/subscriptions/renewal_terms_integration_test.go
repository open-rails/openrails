//go:build integration

package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func TestAcceptedRenewalKeepsItsCatalogBenefitsAndPeriod(t *testing.T) {
	for _, change := range []string{"unchanged empty snapshot", "due reprice", "due plan change"} {
		t.Run(change, func(t *testing.T) {
			f := newRepriceFixture(t)
			ctx := db.WithPSPID(dbtest.WithTestMerchant(context.Background()), f.nmiPSPID)
			subID, railSubID := f.createSubscription(t, ctx, f.lowPriceID)
			targetPrice := f.lowPriceID
			if change == "due reprice" {
				targetPrice = f.highPriceID
				_, err := f.repriceSvc.Reprice(ctx, RepriceRequest{SubscriptionID: subID, ToPriceID: targetPrice, EffectiveAt: f.clock.Now()})
				require.NoError(t, err)
			} else if change == "due plan change" {
				targetPrice = f.otherProductPriceID
				_, err := f.pool.Exec(ctx, `UPDATE billing.products SET entitlements_spec = '{"accepted":null}' WHERE id = (SELECT product_id FROM billing.prices WHERE id = $1)`, targetPrice)
				require.NoError(t, err)
				_, err = f.repriceRepo.CreatePlanChangeReprice(ctx, subID, f.lowPriceID, targetPrice, f.clock.Now(), nil, false)
				require.NoError(t, err)
			}
			var terms RenewalTerms
			require.NoError(t, f.dbi.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				txdb := f.dbi.NewWithPgxTx(tx)
				sub, err := NewSubscriptionRepo(txdb).GetByIDForUpdate(ctx, subID)
				if err != nil {
					return err
				}
				terms, err = PrepareRenewalTerms(ctx, txdb, sub, f.clock.Now(), nil)
				return err
			}))
			require.Equal(t, targetPrice, terms.PriceID)
			subBefore, err := f.subSvc.GetByID(ctx, subID)
			require.NoError(t, err)
			require.Equal(t, f.lowPriceID, subBefore.PriceID, "preparation does not claim a charge completed")
			gateway, client := newFakeNMIPlanGateway(t, f.merchantID, f.nmiPSPID, railSubID, "10.00", "0")
			require.NoError(t, NewNMIPlanPusher(fakeNMIClientSource{client: client}).PushPlanAmount(ctx, subBefore, terms.Currency, terms.Amount))
			wantAmount := "10.00"
			if change == "due reprice" {
				wantAmount = "12.00"
			}
			require.Equal(t, wantAmount, gateway.updateForms[0]["plan_amount"], "frozen native price reaches only this subscription as exact provider decimal")
			require.Equal(t, railSubID, gateway.updateForms[0]["subscription_id"])
			// Both display metadata and granted benefits can change independently of
			// the immutable price. Completion must not fetch either afresh.
			_, err = f.pool.Exec(ctx, `UPDATE billing.products SET entitlements_spec = '{"later":null}', display_name = 'changed later' WHERE id = $1`, terms.ProductID)
			require.NoError(t, err)
			f.clock.Advance(time.Hour)
			params := &RenewMembershipParams{Prepared: &terms, Rail: models.RailNMI, RailSubscriptionID: railSubID, TransactionID: "accepted-" + uuid.NewString(), Amount: terms.Amount, AmountProvided: true, Currency: terms.Currency}
			require.NoError(t, f.lifecycle.RenewMembership(ctx, params))
			subAfter, err := f.subSvc.GetByID(ctx, subID)
			require.NoError(t, err)
			require.Equal(t, terms.PriceID, subAfter.PriceID)
			require.Equal(t, terms.Entitlements, subAfter.EntitlementsSpecSnapshot)
			require.WithinDuration(t, terms.PeriodStart, *subAfter.CurrentPeriodStartsAt, time.Microsecond)
			require.WithinDuration(t, terms.PeriodEnd, *subAfter.CurrentPeriodEndsAt, time.Microsecond)
			var amount int64
			var snapshot map[string]*int
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT amount, entitlements_spec_snapshot FROM billing.payments WHERE transaction_id = $1 AND merchant_id = $2`, params.TransactionID, f.merchantID).Scan(&amount, &snapshot))
			require.Equal(t, terms.Amount, amount)
			require.Equal(t, terms.Entitlements, models.CloneEntitlementsSpec(snapshot))
			require.NoError(t, f.lifecycle.RenewMembership(ctx, params), "exact transaction replay does not require the old period still to be current")
			var payments, notifications int
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id = $1`, subID).Scan(&payments))
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.notifications WHERE customer_id = $1 AND event_type = $2`, terms.CustomerID, models.NotificationPremiumRenewed).Scan(&notifications))
			require.Equal(t, 1, payments)
			require.Equal(t, 1, notifications)
		})
	}
}

func TestAcceptedRenewalCompletesObservedPaymentAndPreservesLaterDunning(t *testing.T) {
	f := newRepriceFixture(t)
	ctx := db.WithPSPID(dbtest.WithTestMerchant(context.Background()), f.nmiPSPID)
	subID, reference := f.createSubscription(t, ctx, f.lowPriceID)
	_, err := f.pool.Exec(ctx, `UPDATE billing.subscriptions SET entitlements_spec_snapshot = '{"paid":null}' WHERE id = $1`, subID)
	require.NoError(t, err)
	var terms RenewalTerms
	require.NoError(t, f.dbi.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := f.dbi.NewWithPgxTx(tx)
		sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subID)
		if err != nil {
			return err
		}
		terms, err = PrepareRenewalTerms(ctx, d, sub, f.clock.Now(), nil)
		return err
	}))
	// Two actual accounts carry identical provider subscription/transaction
	// references. An ambient host PSP cannot select the other account's rows.
	otherPSP := uuid.New()
	_, err = f.pool.Exec(ctx, `INSERT INTO billing.psps (id, merchant_id, rail, environment, account_id, key, archived) VALUES ($1,$2,'nmi','test',$3,$3,false)`, otherPSP, f.merchantID, "other-"+otherPSP.String())
	require.NoError(t, err)
	otherSub, _ := f.createSubscription(t, ctx, f.lowPriceID)
	_, err = f.pool.Exec(ctx, `UPDATE billing.subscriptions SET psp_id=$2, rail_subscription_id=$3 WHERE id=$1`, otherSub, otherPSP, reference)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM billing.payments WHERE psp_id=$1`, otherPSP)
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM billing.subscriptions WHERE psp_id=$1`, otherPSP)
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM billing.psps WHERE id=$1`, otherPSP)
	})
	other, err := f.subSvc.GetByID(ctx, otherSub)
	require.NoError(t, err)
	transaction := "observed-" + uuid.NewString()
	for _, sub := range []struct{ id, customer, psp uuid.UUID }{{subID, terms.CustomerID, f.nmiPSPID}, {otherSub, other.CustomerID, otherPSP}} {
		payment := &models.Payment{ID: uuid.New(), CustomerID: sub.customer, PriceID: terms.PriceID, SubscriptionID: &sub.id, PspID: &sub.psp, Rail: models.RailNMI, TransactionID: transaction, Amount: terms.Amount, ListAmount: terms.Amount, Currency: terms.Currency, Status: payments.PaymentStatusCompletedValue, MoneyMovement: models.MoneyMovementRail, EntitlementsSpecSnapshot: terms.Entitlements, PurchasedAt: f.clock.Now(), CreatedAt: f.clock.Now()}
		_, err := payments.NewPaymentService(f.dbi, f.clock).CreateIfNotExists(db.WithPSPID(ctx, sub.psp), payment)
		require.NoError(t, err)
	}
	params := &RenewMembershipParams{Prepared: &terms, Rail: models.RailNMI, RailSubscriptionID: reference, TransactionID: transaction, Amount: terms.Amount, AmountProvided: true, Currency: terms.Currency}
	require.NoError(t, f.lifecycle.RenewMembership(db.WithPSPID(ctx, otherPSP), params))
	actual, err := f.subSvc.GetByID(ctx, subID)
	require.NoError(t, err)
	require.WithinDuration(t, terms.PeriodEnd, *actual.CurrentPeriodEndsAt, time.Microsecond, "observed payment alone had not completed the accepted period")
	unchanged, err := f.subSvc.GetByID(ctx, otherSub)
	require.NoError(t, err)
	require.Equal(t, other.CurrentPeriodEndsAt, unchanged.CurrentPeriodEndsAt)
	var grants int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.entitlements WHERE source_id=$1 AND entitlement='paid'`, subID).Scan(&grants))
	require.Equal(t, 1, grants)
	// A later failure must not be cleared by replaying this older charge.
	next := f.clock.Now().Add(7 * time.Hour).Truncate(time.Microsecond)
	_, err = f.pool.Exec(ctx, `UPDATE billing.subscriptions SET status='past_due', retry_attempts=7, next_retry_at=$2 WHERE id=$1`, subID, next)
	require.NoError(t, err)
	require.NoError(t, f.lifecycle.RenewMembership(ctx, params))
	later, err := f.subSvc.GetByID(ctx, subID)
	require.NoError(t, err)
	require.Equal(t, models.StatusPastDue, later.Status)
	require.Equal(t, 7, *later.RetryAttempts)
	require.True(t, later.NextRetryAt.Equal(next))
	var notifications int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.notifications WHERE customer_id=$1 AND event_type=$2`, terms.CustomerID, models.NotificationPremiumRenewed).Scan(&notifications))
	require.Equal(t, 1, notifications)
}
