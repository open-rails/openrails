//go:build integration

package money_test

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/intents"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/testfixture"
	"github.com/stretchr/testify/require"
)

func TestEngineCollectionAdmissionPrototype(t *testing.T) {
	for _, gap := range []bool{false, true} {
		t.Run(map[bool]string{false: "on_time", true: "missed_periods"}[gap], func(t *testing.T) {
			e := newNMIReceiptEnv(t)
			mid := dbtest.TestMerchantID.UUID()
			custody, product, price, sub := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			*e.custodian = custody
			now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
			previous := now
			if gap {
				previous = now.Add(-100 * 24 * time.Hour)
			}
			_, err := e.pool.Exec(e.ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings) VALUES($1,$2,$3,'hyperswitch','test',$4,'{"profile_id":"profile-engine","public_api_key":"public-engine"}')`, custody, mid, custody.String(), "engine-"+custody.String())
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET charge_via='pan_proxy',custodian='hyperswitch',custodian_id=$2,rail_customer_ref='customer-engine',rail_method_ref='method-engine',stored_credential_recurring_ref='qualified-recurring' WHERE id=$1`, e.method, custody)
			require.NoError(t, err)
			method := e.methodRow(t)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name,entitlements_spec) VALUES($1,$2,$3,'Engine','{"engine":null}')`, product, mid, product.String())
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,9990000,'USD',720,true)`, price, mid, product)
			require.NoError(t, err)
			initial := testfixture.EngineMembership(t, e.ctx, e.db, subscriptions.InitialMembershipTerms{CollectionPolicy: models.CollectionPolicyEngine, SubscriptionID: sub, PaymentID: uuid.New(), CustomerID: e.payer.UUID(), PSPID: method.PspID, ProductID: product, PriceID: price, PaymentMethodID: e.method, ProductName: "Engine", Amount: 9990000, RecurringAmount: 9990000, Currency: "USD", AcceptedAt: previous.Add(-30 * 24 * time.Hour), PeriodStart: previous.Add(-30 * 24 * time.Hour), PeriodEnd: previous, Entitlements: map[string]*int{"engine": nil}})
			t.Cleanup(func() {
				c := context.WithoutCancel(e.ctx)
				_, _ = e.pool.Exec(c, `DELETE FROM billing.rail_intents WHERE subscription_id=$1`, sub)
				_, _ = e.pool.Exec(c, `DELETE FROM billing.subscriptions WHERE id=$1`, sub)
				_, _ = e.pool.Exec(c, `DELETE FROM billing.prices WHERE id=$1`, price)
				_, _ = e.pool.Exec(c, `DELETE FROM billing.products WHERE id=$1`, product)
			})
			require.NoError(t, e.svc.SetHyperSwitchDeployment("http://127.0.0.1:1"))
			if !gap {
				for _, scenario := range []struct{ name, sql string }{
					{"missing paid owner", `UPDATE billing.rail_intents SET status='unknown_needs_verify' WHERE id=$1`},
					{"pruned accepted terms", `UPDATE billing.rail_intents SET payload='{}' WHERE id=$1`},
					{"missing receipt", `UPDATE billing.rail_intents SET result_evidence='{}' WHERE id=$1`},
					{"changed accepted benefits", `UPDATE billing.rail_intents SET payload=jsonb_set(payload,'{terms,entitlements}','{"unaccepted":null}') WHERE id=$1`},
					{"changed accepted account", `UPDATE billing.rail_intents SET payload=jsonb_set(payload,'{terms,psp_id}',to_jsonb('00000000-0000-4000-8000-000000000001'::text)) WHERE id=$1`},
				} {
					t.Run(scenario.name, func(t *testing.T) {
						_, err = e.pool.Exec(e.ctx, scenario.sql, initial.ID)
						require.NoError(t, err)
						defer func() {
							_, restoreErr := e.pool.Exec(e.ctx, `UPDATE billing.rail_intents SET status=$2,payload=$3,result_evidence=$4 WHERE id=$1`, initial.ID, initial.Status, initial.Payload, initial.ResultEvidence)
							require.NoError(t, restoreErr)
						}()
						_, err = e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
						require.Error(t, err)
					})
				}
				duplicate := uuid.New()
				_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.rail_intents(id,merchant_id,rail,intent_type,idempotency_key,status,origin,psp_id,price_id,payload,result_evidence,actor,executed_at) SELECT $2,merchant_id,rail,intent_type,idempotency_key||':ambiguous',status,origin,psp_id,price_id,payload,result_evidence,actor,executed_at FROM billing.rail_intents WHERE id=$1`, initial.ID, duplicate)
				require.NoError(t, err)
				_, err = e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
				require.ErrorContains(t, err, "ambiguous")
				_, err = e.pool.Exec(e.ctx, `DELETE FROM billing.rail_intents WHERE id=$1`, duplicate)
				require.NoError(t, err)
				_, err = e.pool.Exec(e.ctx, `UPDATE billing.payments SET amount=1000000 WHERE transaction_id=$1`, "initial-"+sub.String())
				require.NoError(t, err)
				_, err = e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
				require.Error(t, err, "receipt without matching original payment is not billing authority")
				_, err = e.pool.Exec(e.ctx, `UPDATE billing.payments SET amount=9990000 WHERE transaction_id=$1`, "initial-"+sub.String())
				require.NoError(t, err)
			}
			_, err = e.pool.Exec(e.ctx, `UPDATE billing.prices SET amount=1000000,access_duration_hours=24 WHERE id=$1`, price)
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `UPDATE billing.products SET entitlements_spec='{"unaccepted":null}' WHERE id=$1`, product)
			require.NoError(t, err)
			// Only an explicit existing scheduled change selects fresh target
			// terms. Removing it returns to the same original agreement.
			changed := uuid.New()
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,1230000,'USD',48,true)`, changed, mid, product)
			require.NoError(t, err)
			for _, changeKind := range []string{"tier", "reprice"} {
				reprice := uuid.New()
				if changeKind == "tier" {
					_, err = e.pool.Exec(e.ctx, `UPDATE billing.subscriptions SET scheduled_price_id=$2 WHERE id=$1`, sub, changed)
				} else {
					_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.subscription_reprices(id,merchant_id,subscription_id,from_price_id,to_price_id,effective_at) VALUES($1,$2,$3,$4,$5,$6)`, reprice, mid, sub, price, changed, now)
				}
				require.NoError(t, err)
				require.NoError(t, e.db.MerchantTx(e.ctx, func(ctx context.Context, tx pgx.Tx) error {
					d := e.db.NewWithPgxTx(tx)
					row, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, sub)
					if err != nil {
						return err
					}
					accepted, err := intents.PrepareEngineRenewalTerms(ctx, d, row, now)
					if err != nil {
						return err
					}
					require.Equal(t, int64(1230000), accepted.Amount)
					require.Equal(t, 48*time.Hour, accepted.PeriodEnd.Sub(accepted.PeriodStart))
					if changeKind == "tier" {
						require.Equal(t, &changed, accepted.ScheduledPriceID)
					} else {
						require.Equal(t, &reprice, accepted.RepriceID)
						require.Equal(t, map[string]*int{"engine": nil}, accepted.Entitlements, "same-product reprice retains accepted benefits")
					}
					return nil
				}))
				if changeKind == "tier" {
					_, err = e.pool.Exec(e.ctx, `UPDATE billing.subscriptions SET scheduled_price_id=NULL WHERE id=$1`, sub)
				} else {
					_, err = e.pool.Exec(e.ctx, `DELETE FROM billing.subscription_reprices WHERE id=$1`, reprice)
				}
				require.NoError(t, err)
			}
			var wg sync.WaitGroup
			ids := make([]uuid.UUID, 2)
			errs := make([]error, 2)
			for i := range 2 {
				wg.Go(func() {
					row, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
					ids[i], errs[i] = row.ID, err
				})
			}
			wg.Wait()
			for _, err := range errs {
				require.NoError(t, err)
			}
			require.Equal(t, ids[0], ids[1])
			later, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now.Add(90*24*time.Hour))
			require.NoError(t, err)
			require.Equal(t, ids[0], later.ID)
			accepted, err := subscriptions.DecodeSubscriptionCollectionPayload(later)
			require.NoError(t, err)
			require.Equal(t, int64(9990000), accepted.Renewal.Amount)
			require.Equal(t, 720*time.Hour, accepted.Renewal.PeriodEnd.Sub(accepted.Renewal.PeriodStart))
			require.Equal(t, map[string]*int{"engine": nil}, accepted.Renewal.Entitlements)
			require.True(t, accepted.PreviousPeriodEnd.Equal(previous))
			require.True(t, accepted.Renewal.PeriodStart.Equal(now))
			require.True(t, accepted.Renewal.PeriodEnd.Equal(now.Add(30*24*time.Hour)))
			require.True(t, accepted.AcceptedAt.Equal(now))
			require.Equal(t, "qualified-recurring", accepted.Instrument.StoredCredentialRecurringRef)
			// Custody/period drift cannot replace the unresolved owner or admit another
			// payment. The later execution/completion stage must handle relevance.
			// Synthetic uncertainty qualifies preservation only: the engine has no
			// charge executor yet and this fixture never contacts a provider.
			_, err = e.pool.Exec(e.ctx, `UPDATE billing.rail_intents SET status='unknown_needs_verify',result_evidence='{"submission":"synthetic uncertainty"}' WHERE id=$1`, later.ID)
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `UPDATE billing.subscriptions SET status='unknown' WHERE id=$1`, sub)
			require.NoError(t, err)
			unknown, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now.Add(time.Hour))
			require.NoError(t, err)
			require.Equal(t, later.ID, unknown.ID)
			require.JSONEq(t, string(later.Payload), string(unknown.Payload))
			lifecycle := subscriptions.NewSubscriptionLifecycleService(e.db, nil, nil, nil, nil, nil, clockwork.NewFakeClockAt(now))
			require.NoError(t, lifecycle.CancelMembership(e.ctx, &subscriptions.CancelMembershipParams{SubscriptionID: &sub, CancelType: models.CancelTypeUser}))
			replay, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now.Add(120*24*time.Hour))
			require.NoError(t, err)
			require.Equal(t, ids[0], replay.ID)
			require.Equal(t, "unknown_needs_verify", replay.Status)
			require.JSONEq(t, `{"submission":"synthetic uncertainty"}`, string(replay.ResultEvidence))
			require.JSONEq(t, string(later.Payload), string(replay.Payload))
			// With no unresolved owner, cancellation forbids a NEW operation.
			untouched := uuid.New()
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,collection_policy,rail_subscription_id,payment_method_id,status,current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type) SELECT $2,merchant_id,customer_id,product_id,price_id,psp_id,rail,collection_policy,rail_subscription_id,payment_method_id,'cancelled',current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type FROM billing.subscriptions WHERE id=$1`, sub, untouched)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = e.pool.Exec(context.WithoutCancel(e.ctx), `DELETE FROM billing.subscriptions WHERE id=$1`, untouched)
			})
			_, err = e.svc.AdmitDueSubscriptionCollection(e.ctx, untouched, now.Add(120*24*time.Hour))
			require.ErrorContains(t, err, "not due")
			var operations, payments int
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id IN ($1,$2)`, sub, untouched).Scan(&operations))
			require.Equal(t, 1, operations)
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id IN ($1,$2) AND attempt_kind='renewal'`, sub, untouched).Scan(&payments))
			require.Zero(t, payments)
			e.gateway.mu.Lock()
			sends := e.gateway.sends
			e.gateway.mu.Unlock()
			require.Zero(t, sends, "admission performs no financial provider call")
		})
	}
}
