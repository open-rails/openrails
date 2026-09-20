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
				_, err := f.pool.Exec(ctx, `UPDATE openrails.products SET entitlements_spec = '{"accepted":null}' WHERE id = (SELECT product_id FROM openrails.prices WHERE id = $1)`, targetPrice)
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
				terms, err = PrepareRenewalTerms(ctx, txdb, sub, f.clock.Now())
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
			_, err = f.pool.Exec(ctx, `UPDATE openrails.products SET entitlements_spec = '{"later":null}', display_name = 'changed later' WHERE id = $1`, terms.ProductID)
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
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT amount, entitlements_spec_snapshot FROM openrails.payments WHERE transaction_id = $1 AND merchant_id = $2`, params.TransactionID, f.merchantID).Scan(&amount, &snapshot))
			require.Equal(t, terms.Amount, amount)
			require.Equal(t, terms.Entitlements, models.CloneEntitlementsSpec(snapshot))
			require.NoError(t, f.lifecycle.RenewMembership(ctx, params), "exact transaction replay does not require the old period still to be current")
			var payments, notifications int
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE subscription_id = $1`, subID).Scan(&payments))
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM openrails.notifications WHERE customer_id = $1 AND event_type = $2`, terms.CustomerID, models.NotificationPremiumRenewed).Scan(&notifications))
			require.Equal(t, 1, payments)
			require.Equal(t, 1, notifications)
		})
	}
}
