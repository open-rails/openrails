//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func TestEngineProducerScopesEachEligibleMerchant(t *testing.T) {
	d := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	admin := dbtest.SharedSuperuserPGXPool(t)
	now := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	type subject struct {
		mid, sub, customer, psp uuid.UUID
		eligible                bool
	}
	var subjects []subject
	for _, mode := range []string{"ready_a", "ready_b", "archived", "parked"} {
		s := subject{uuid.New(), uuid.New(), uuid.New(), uuid.New(), mode == "ready_a" || mode == "ready_b"}
		subjects = append(subjects, s)
		product, price, custodian, method := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		_, err := admin.Exec(t.Context(), `INSERT INTO billing.merchants(id,slug) VALUES($1,$2)`, s.mid, "engine-producer-"+s.mid.String())
		require.NoError(t, err)
		_, err = admin.Exec(t.Context(), `INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, s.mid, s.customer)
		require.NoError(t, err)
		_, err = admin.Exec(t.Context(), `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,archived) VALUES($1,$2,'nmi','test',$3,$4)`, s.psp, s.mid, s.psp.String(), mode == "archived")
		require.NoError(t, err)
		_, err = admin.Exec(t.Context(), `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings) VALUES($1,$2,$3,'hyperswitch','test',$3,'{"profile_id":"scope","public_api_key":"synthetic"}')`, custodian, s.mid, custodian.String())
		require.NoError(t, err)
		parked := ""
		if mode == "parked" {
			parked = "unavailable"
		}
		_, err = admin.Exec(t.Context(), `INSERT INTO billing.payment_methods(id,merchant_id,customer_id,psp_id,rail,custodian,custodian_id,initial_transaction_id,rail_customer_ref,rail_method_ref,stored_credential_recurring_ref,park_reason) VALUES($1,$2,$3,$4,'nmi','hyperswitch',$5,'initial','customer','method','recurring',$6)`, method, s.mid, s.customer, s.psp, custodian, parked)
		require.NoError(t, err)
		_, err = admin.Exec(t.Context(), `INSERT INTO billing.products(id,merchant_id,key,display_name,entitlements_spec) VALUES($1,$2,$3,'Scope','{}')`, product, s.mid, product.String())
		require.NoError(t, err)
		_, err = admin.Exec(t.Context(), `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,5000000,'USD',true,720)`, price, s.mid, product)
		require.NoError(t, err)
		prior := now.Add(-time.Hour)
		if !s.eligible {
			prior = now.Add(-365 * 24 * time.Hour)
		}
		_, err = admin.Exec(t.Context(), `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,collection_policy,rail_subscription_id,status,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,$7,'nmi','engine','','active',$8,$9)`, s.sub, s.mid, s.customer, product, price, s.psp, method, prior.Add(-30*24*time.Hour), prior)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = admin.Exec(context.Background(), `DELETE FROM billing.rail_intents WHERE merchant_id=$1`, s.mid)
			_, _ = admin.Exec(context.Background(), `UPDATE billing.subscriptions SET deleted_at=now() WHERE merchant_id=$1`, s.mid)
		})
	}
	ids, err := d.GenDirectory().ListDueDunningMerchants(t.Context(), gen.ListDueDunningMerchantsParams{Rails: []string{"nmi"}, Now: now, MerchantLimit: 2, IncludeEngine: true})
	require.NoError(t, err)
	require.Len(t, ids, 2)
	for _, id := range ids {
		require.Contains(t, []uuid.UUID{subjects[0].mid, subjects[1].mid}, *id, "ineligible older obligations must not occupy the bounded prefix")
	}
	svc := money.NewMoneyService(d)
	require.NoError(t, svc.SetHyperSwitchDeployment("http://127.0.0.1:1"))
	worker := DunningWorker{DB: d, Config: &config.Config{Env: "dev", ProviderWriteMode: config.ProviderWriteModeFull}, Clock: clockwork.NewFakeClockAt(now), EngineCollections: svc}
	require.NoError(t, worker.Work(t.Context(), &river.Job[DunningArgs]{}))
	require.NoError(t, worker.Work(t.Context(), &river.Job[DunningArgs]{}))
	for _, s := range subjects {
		require.NoError(t, d.RunInMerchantScope(t.Context(), merchant.ID(s.mid), "engine scope proof", func(ctx context.Context) error {
			row, err := d.Gen(ctx).GetUnresolvedSubscriptionCollection(ctx, gen.GetUnresolvedSubscriptionCollectionParams{MerchantID: s.mid, SubscriptionID: s.sub})
			if !s.eligible {
				require.Error(t, err)
				return nil
			}
			require.NoError(t, err)
			p, err := subscriptions.DecodeSubscriptionCollectionPayload(row)
			require.NoError(t, err)
			require.Equal(t, s.mid, row.MerchantID)
			require.Equal(t, s.customer, p.Renewal.CustomerID)
			require.Equal(t, s.psp, p.Instrument.PSPID)
			var count int
			require.NoError(t, d.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE merchant_id=$1 AND subscription_id=$2`, s.mid, s.sub).Scan(&count))
			require.Equal(t, 1, count)
			return nil
		}))
	}
}
