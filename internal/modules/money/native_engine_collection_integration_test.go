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
	"github.com/open-rails/openrails/internal/testfixture"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestNativeEngineRecurringCollectionOwnsOnlyNewAgreement(t *testing.T) {
	for _, mode := range []string{"paid", "unknown_then_paid", "cancel_unknown", "declined_customer_retry"} {
		t.Run(mode, func(t *testing.T) {
			e := newNMIReceiptEnv(t)
			now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
			clock := clockwork.NewFakeClockAt(now)
			e.svc.SetClock(clock)
			mid := dbtest.TestMerchantID.UUID()
			principal := billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: mid.String(), SubjectID: e.payer.UUID().String()}
			product, price, sub, legacy := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			_, err := e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET rail_method_ref='engine-billing',stored_credential_recurring_ref='' WHERE id=$1`, e.method)
			require.NoError(t, err)
			method := e.methodRow(t)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$1::uuid::text,'Engine native')`, product, mid)
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,9990000,'USD',true,720)`, price, mid, product)
			require.NoError(t, err)
			testfixture.EngineMembership(t, e.ctx, e.db, subscriptions.InitialMembershipTerms{CollectionPolicy: models.CollectionPolicyEngine, SubscriptionID: sub, PaymentID: uuid.New(), CustomerID: e.payer.UUID(), PSPID: method.PspID, ProductID: product, PriceID: price, PaymentMethodID: e.method, ProductName: "Engine native", Amount: 9990000, RecurringAmount: 9990000, Currency: "USD", AcceptedAt: now.Add(-30 * 24 * time.Hour), PeriodStart: now.Add(-30 * 24 * time.Hour), PeriodEnd: now})
			for _, r := range []struct {
				id                     uuid.UUID
				policy, remote, status string
			}{{legacy, "provider_dunning", "legacy-schedule", "cancelled"}} {
				_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,collection_policy,rail_subscription_id,status,current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type) VALUES($1,$2,$3,$4,$5,$6,$7,'nmi',$8,$9,$10::text::billing.subscription_status,$11,$12,CASE WHEN $10='cancelled' THEN $11::timestamptz END,CASE WHEN $10='cancelled' THEN 'user' END)`, r.id, mid, e.payer.UUID(), product, price, method.PspID, e.method, r.policy, r.remote, r.status, now.Add(-30*24*time.Hour), now)
				require.NoError(t, err)
			}
			_, err = e.svc.AdmitDueSubscriptionCollection(e.ctx, legacy, now)
			require.ErrorContains(t, err, "engine-owned")
			lifecycle := subscriptions.NewSubscriptionLifecycleService(e.db, nil, nil, nil, nil, nil, clock)
			if mode == "paid" {
				_, err = e.pool.Exec(e.ctx, `UPDATE billing.subscriptions SET payment_method_id=NULL WHERE id=$1`, sub)
				require.NoError(t, err)
				require.NoError(t, lifecycle.UpdateEnginePaymentMethod(e.ctx, sub, e.payer.UUID(), e.method))
				require.Error(t, lifecycle.UpdateEnginePaymentMethod(e.ctx, legacy, e.payer.UUID(), e.method), "engine binding cannot mutate a legacy agreement")
			}
			e.svc.EngineAdmissionHold = true
			_, err = e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
			require.ErrorContains(t, err, "admission is held")
			e.svc.EngineAdmissionHold = false
			op, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
			require.NoError(t, err)
			e.svc.EngineAdmissionHold = true
			same, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
			require.NoError(t, err)
			require.Equal(t, op.ID, same.ID)
			require.ErrorIs(t, lifecycle.UpdateEnginePaymentMethod(e.ctx, sub, e.payer.UUID(), e.method), subscriptions.ErrRebillTermsCommitted)
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
			if mode == "declined_customer_retry" {
				e.gateway.mu.Lock()
				e.gateway.saleResponse = "response=2&response_code=200"
				e.gateway.mu.Unlock()
			}
			e.plane.Config.EngineAdmissionHold = true
			outcome := handler.Execute(e.ctx, op)
			require.Equal(t, intents.OutcomeParked, outcome.Class)
			e.plane.Config.EngineAdmissionHold = false
			outcome = handler.Execute(e.ctx, op)
			if mode == "declined_customer_retry" {
				require.Equal(t, intents.OutcomeTerminal, outcome.Class, outcome.Reason)
				_, err = e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
				require.Error(t, err)
				_, _, err = e.svc.AdmitCustomerSubscriptionCollection(e.ctx, sub, e.payer.UUID(), "retry-key", &e.method, principal)
				require.ErrorContains(t, err, "held")
				e.svc.EngineAdmissionHold = false
				retry, replayed, err := e.svc.AdmitCustomerSubscriptionCollection(e.ctx, sub, e.payer.UUID(), "retry-key", &e.method, principal)
				require.NoError(t, err)
				require.False(t, replayed)
				terms, err := subscriptions.DecodeSubscriptionCollectionPayload(retry)
				require.NoError(t, err)
				require.Equal(t, "customer", string(terms.Initiator))
				e.gateway.mu.Lock()
				e.gateway.saleResponse = "response=1&response_code=100&transactionid=" + txn
				e.gateway.mu.Unlock()
				e.gateway.orderSale(terms.OrderReference, txn)
				e.gateway.payment(txn, method.RailCustomerRef, "9.99", "USD")
				retry, claimed, err = intents.NewStore(e.db).ClaimByID(e.ctx, retry.ID, now, now.Add(time.Minute))
				require.NoError(t, err)
				require.True(t, claimed)
				outcome = handler.Execute(e.ctx, retry)
				require.Equal(t, intents.OutcomeSucceeded, outcome.Class, outcome.Reason)
				e.svc.EngineAdmissionHold = true
				again, replayed, err := e.svc.AdmitCustomerSubscriptionCollection(e.ctx, sub, e.payer.UUID(), "retry-key", &e.method, principal)
				require.NoError(t, err)
				require.True(t, replayed)
				require.Equal(t, retry.ID, again.ID)
				requireEngineArchiveValues(t, e, retry.ID)
				e.gateway.mu.Lock()
				require.Equal(t, 2, e.gateway.sends)
				require.Equal(t, []string{"merchant", "customer"}, e.gateway.saleInitiators)
				e.gateway.mu.Unlock()
				return
			}
			if mode != "paid" {
				require.Equal(t, intents.OutcomeAmbiguous, outcome.Class, outcome.Reason)
				_, _, err = e.svc.AdmitCustomerSubscriptionCollection(e.ctx, sub, e.payer.UUID(), "not-accepted-while-uncertain", nil, principal)
				require.ErrorIs(t, err, intents.ErrRebillInProgress)
				e.plane.Config.EngineAdmissionHold = true
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
			requireEngineArchiveValues(t, e, op.ID)
			e.gateway.mu.Lock()
			require.Equal(t, 1, e.gateway.sends)
			require.Equal(t, []string{"engine-billing"}, e.gateway.saleBillingIDs)
			e.gateway.mu.Unlock()
			var count int
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1 AND attempt_kind='renewal' AND status='completed' AND token_type='psp_token'`, sub).Scan(&count))
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
