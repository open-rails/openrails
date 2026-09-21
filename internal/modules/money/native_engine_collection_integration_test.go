//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestNativeEngineRecurringCollectionOwnsOnlyNewAgreement(t *testing.T) {
	for _, mode := range []string{"paid", "unknown_then_paid", "cancel_unknown"} {
		t.Run(mode, func(t *testing.T) {
			e := newNMIReceiptEnv(t)
			now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
			clock := clockwork.NewFakeClockAt(now)
			mid := dbtest.TestMerchantID.UUID()
			product, price, sub, legacy := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			_, err := e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET rail_method_ref='engine-billing',stored_credential_recurring_ref='original-recurring' WHERE id=$1`, e.method)
			require.NoError(t, err)
			method := e.methodRow(t)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$1::uuid::text,'Engine native')`, product, mid)
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,9990000,'USD',true,720)`, price, mid, product)
			require.NoError(t, err)
			for _, r := range []struct {
				id                     uuid.UUID
				policy, remote, status string
			}{{sub, "engine", "", "active"}, {legacy, "provider_dunning", "legacy-schedule", "cancelled"}} {
				_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,collection_policy,rail_subscription_id,status,current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type) VALUES($1,$2,$3,$4,$5,$6,$7,'nmi',$8,$9,$10::text::billing.subscription_status,$11,$12,CASE WHEN $10='cancelled' THEN $11::timestamptz END,CASE WHEN $10='cancelled' THEN 'user' END)`, r.id, mid, e.payer.UUID(), product, price, method.PspID, e.method, r.policy, r.remote, r.status, now.Add(-30*24*time.Hour), now)
				require.NoError(t, err)
			}
			_, err = e.svc.AdmitDueSubscriptionCollection(e.ctx, legacy, now)
			require.ErrorContains(t, err, "engine-owned")
			op, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
			require.NoError(t, err)
			same, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
			require.NoError(t, err)
			require.Equal(t, op.ID, same.ID)
			op, claimed, err := intents.NewStore(e.db).ClaimByID(e.ctx, op.ID, now, now.Add(time.Minute))
			require.NoError(t, err)
			require.True(t, claimed)
			p, err := subscriptions.DecodeSubscriptionCollectionPayload(op)
			require.NoError(t, err)
			require.Nil(t, p.Instrument.CustodianID)
			handler := money.NewSubscriptionCollectionHandler(e.db, e.plane, e.plane.Config, clock)
			txn := "native-engine-" + op.ID.String()
			if mode == "paid" {
				e.gateway.mu.Lock()
				e.gateway.saleResponse = "response=1&response_code=100&transactionid=" + txn
				e.gateway.mu.Unlock()
				e.gateway.orderSale(p.OrderReference, txn)
				e.gateway.payment(txn, method.RailCustomerRef, "9.99", "USD")
			}
			outcome := handler.Execute(e.ctx, op)
			if mode != "paid" {
				require.Equal(t, intents.OutcomeAmbiguous, outcome.Class, outcome.Reason)
				outcome = handler.Verify(e.ctx, op)
				require.Equal(t, intents.OutcomeAmbiguous, outcome.Class)
				if mode == "cancel_unknown" {
					lc := subscriptions.NewSubscriptionLifecycleService(e.db, nil, nil, nil, nil, nil, clock)
					require.NoError(t, lc.CancelMembership(e.ctx, &subscriptions.CancelMembershipParams{SubscriptionID: &sub, CancelType: models.CancelTypeMerchant, RevokeAccess: true}))
				}
				e.gateway.orderSale(p.OrderReference, txn)
				e.gateway.payment(txn, method.RailCustomerRef, "9.99", "USD")
				outcome = handler.Verify(e.ctx, op)
			}
			require.Equal(t, intents.OutcomeSucceeded, outcome.Class, outcome.Reason)
			outcome = handler.Verify(e.ctx, op)
			require.Equal(t, intents.OutcomeSucceeded, outcome.Class, outcome.Reason)
			e.gateway.mu.Lock()
			require.Equal(t, 1, e.gateway.sends)
			require.Equal(t, []string{"engine-billing"}, e.gateway.saleBillingIDs)
			e.gateway.mu.Unlock()
			var count int
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1 AND status='completed' AND token_type='psp_token'`, sub).Scan(&count))
			require.Equal(t, 1, count)
			var status, policy, remote string
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT status,collection_policy,rail_subscription_id FROM billing.subscriptions WHERE id=$1`, legacy).Scan(&status, &policy, &remote))
			require.Equal(t, "provider_dunning", policy)
			require.Equal(t, "legacy-schedule", remote)
			if mode == "cancel_unknown" {
				require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT status FROM billing.subscriptions WHERE id=$1`, sub).Scan(&status))
				require.Equal(t, "cancelled", status)
			}
		})
	}
}
