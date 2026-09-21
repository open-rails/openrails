//go:build integration

package money_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
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
			_, err = e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET custodian='hyperswitch',custodian_id=$2,rail_customer_ref='customer-engine',rail_method_ref='method-engine',stored_credential_recurring_ref='qualified-recurring' WHERE id=$1`, e.method, custody)
			require.NoError(t, err)
			method := e.methodRow(t)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name,entitlements_spec) VALUES($1,$2,$3,'Engine','{"engine":null}')`, product, mid, product.String())
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,9990000,'USD',720,true)`, price, mid, product)
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,collection_policy,rail_subscription_id,payment_method_id,status,current_period_starts_at,current_period_ends_at,entitlements_spec_snapshot) VALUES($1,$2,$3,$4,$5,$6,'nmi','engine','',$7,'active',$8,$9,'{"engine":null}')`, sub, mid, e.payer.UUID(), product, price, method.PspID, e.method, previous.Add(-30*24*time.Hour), previous)
			require.NoError(t, err)
			t.Cleanup(func() {
				c := context.WithoutCancel(e.ctx)
				_, _ = e.pool.Exec(c, `DELETE FROM billing.rail_intents WHERE subscription_id=$1`, sub)
				_, _ = e.pool.Exec(c, `DELETE FROM billing.subscriptions WHERE id=$1`, sub)
				_, _ = e.pool.Exec(c, `DELETE FROM billing.prices WHERE id=$1`, price)
				_, _ = e.pool.Exec(c, `DELETE FROM billing.products WHERE id=$1`, product)
			})
			require.NoError(t, e.svc.SetHyperSwitchDeployment("http://127.0.0.1:1"))
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
			require.True(t, accepted.PreviousPeriodEnd.Equal(previous))
			require.True(t, accepted.Renewal.PeriodStart.Equal(now))
			require.True(t, accepted.Renewal.PeriodEnd.Equal(now.Add(30*24*time.Hour)))
			require.True(t, accepted.AcceptedAt.Equal(now))
			require.Equal(t, "qualified-recurring", accepted.Instrument.StoredCredentialRecurringRef)
			// Custody/period drift cannot replace the unresolved owner or admit another
			// payment. The later execution/completion stage must handle relevance.
			_, err = e.pool.Exec(e.ctx, `UPDATE billing.subscriptions SET status='cancelled',cancelled_at=$2,cancel_type='user' WHERE id=$1`, sub, now)
			require.NoError(t, err)
			replay, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now.Add(120*24*time.Hour))
			require.NoError(t, err)
			require.Equal(t, ids[0], replay.ID)
			e.gateway.mu.Lock()
			sends := e.gateway.sends
			e.gateway.mu.Unlock()
			require.Zero(t, sends, "admission performs no financial provider call")
		})
	}
}
