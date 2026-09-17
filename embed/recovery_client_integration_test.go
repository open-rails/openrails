//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestRecoveryClientAcrossTransports(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	remote := h.StartStandalone("USD")
	host := h.StartEmbeddedHost("USD")
	// A SaaS process serves several merchants using distinct bound clients.
	multi, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, multi.Close(context.Background())) })
	multiClient, err := multi.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)
	require.NoError(t, multiClient.Verify(ctx))

	local, err := host.Runtime().Client()
	require.NoError(t, err)
	// SaaS: a hosted merchant on the shared engine behind the real hosted
	// control plane, integrating with an owner-minted API key.
	hosted := h.StartHosted("USD")
	tenant := hosted.ProvisionMerchant(hosted.RegisterUser("owner"), "recovery-"+uuid.NewString()[:8])
	deployments := []struct {
		name     string
		client   *openrails.Client
		merchant merchant.ID
	}{
		{"embedded", local, dbtest.TestMerchantID},
		{"hosted_http", host.Client(), dbtest.TestMerchantID},
		{"standalone", remote.Client(), dbtest.TestMerchantID},
		{"multi_merchant", multiClient, dbtest.TestMerchantID},
		{"saas", tenant.Client(), tenant.ID},
	}
	for _, d := range deployments {
		client := d.client
		t.Run(d.name, func(t *testing.T) {
			customer, product, price, psp, method, sub := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			mid := d.merchant.UUID()
			now, end := time.Now().UTC(), time.Now().UTC().Add(48*time.Hour)
			exec := func(sql string, args ...any) { _, err := h.Pool().Exec(ctx, sql, args...); require.NoError(t, err) }
			exec(`INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
			exec(`INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Recovery plan')`, product, mid, product.String())
			exec(`INSERT INTO openrails.prices(id,merchant_id,product_id,key,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,$4,1000000,'USD',true,720)`, price, mid, product, price.String())
			exec(`INSERT INTO openrails.psps(id,merchant_id,rail,environment,account_id,key) VALUES($1,$2,'nmi','test',$3,$3)`, psp, mid, psp.String())
			exec(`INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,psp_id,rail,initial_transaction_id,last_four,card_type) VALUES($1,$2,$3,$4,'nmi','test-anchor','4242','visa')`, method, mid, customer, psp)
			exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,payment_method_id,current_period_starts_at,current_period_ends_at,cancelled_at,cancel_type,deletion_scheduled_at) VALUES($1,$2,$3,$4,$5,$6,'nmi','active',$7,$8,$9,$10,NULL,NULL,NULL)`, sub, mid, customer, product, price, psp, sub.String(), method, now, end)
			_, err := client.DeletePaymentMethod(ctx, customer.String(), method.String())
			require.ErrorIs(t, err, openrails.ErrConflict, "active subscription blocks deleting its card before any provider mutation")
			_, err = client.DeletePaymentMethod(ctx, uuid.NewString(), method.String())
			require.ErrorIs(t, err, openrails.ErrDenied, "merchant mutation still checks the addressed customer's ownership")
			require.NoError(t, client.CancelSubscription(ctx, sub.String(), openrails.CancelSubscriptionRequest{Reason: "customer request"}))
			var intents int
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.rail_intents WHERE merchant_id=$1 AND subscription_id=$2 AND intent_type='nmi_delete_subscription'`, mid, sub).Scan(&intents))
			require.Equal(t, 1, intents, "cancel acceptance and remote intent must commit together")
			page, err := client.ListSubscriptions(ctx, openrails.SubscriptionFilter{CustomerID: customer.String(), Status: "cancelled", PageOptions: openrails.PageOptions{Limit: 1}})
			require.NoError(t, err)
			require.Len(t, page.Data, 1)
			require.EqualValues(t, 1, page.Total)
			require.False(t, page.HasMore)
			row := page.Data[0]
			require.Equal(t, api.FormatSubscriptionID(sub), row.ID)
			require.Equal(t, psp.String(), row.PSPID)
			require.True(t, row.Resumable)
			require.True(t, row.CancelScheduled)
			require.NotNil(t, row.Price)
			require.EqualValues(t, 1000000, row.Price.Amount)
			read, err := client.GetSubscription(ctx, row.ID)
			require.NoError(t, err)
			require.Equal(t, row.ID, read.ID)
			methods, err := client.ListPaymentMethods(ctx, customer.String(), openrails.PageOptions{Limit: 1})
			require.NoError(t, err)
			require.Len(t, methods.Data, 1)
			require.EqualValues(t, 1, methods.Total)
			require.Equal(t, psp.String(), methods.Data[0].PSPID)
			require.Equal(t, "4242", *methods.Data[0].Card.Last4)
			none, err := client.ListPaymentMethods(ctx, customer.String(), openrails.PageOptions{Limit: 1, Offset: 1})
			require.NoError(t, err)
			require.Empty(t, none.Data)
			require.NotNil(t, none.Data)
			// Acceptance is a durable job. No provider endpoint is invoked by this test.
			require.NoError(t, client.ResumeSubscription(ctx, row.ID))
			require.NoError(t, client.ResumeSubscription(ctx, row.ID))
			var jobs int
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT count(*) FROM river_job WHERE args->>'subscription_id'=$1 AND kind=$2`, sub.String(), riverjobs.KindSubscriptionResume).Scan(&jobs))
			require.Equal(t, 1, jobs)
			_, err = client.GetSubscription(ctx, uuid.NewString())
			require.ErrorIs(t, err, openrails.ErrNotFound)
			_, err = client.ListPaymentMethods(ctx, customer.String(), openrails.PageOptions{Limit: 101})
			require.ErrorIs(t, err, openrails.ErrInvalid)
		})
	}
	// Catalog-read selects the fixed viewer role; support legitimately has
	// subscription-update authority and is not a read-only principal.
	token := remote.MintAPIKey(dbtest.TestMerchantSlug, "recovery-readonly", []string{permissions.MerchantSubscriptionsRead, permissions.MerchantCatalogRead})
	reader, err := openrails.NewRemote(remote.BaseURL, openrails.WithAPIKey(token))
	require.NoError(t, err)
	require.ErrorIs(t, reader.ResumeSubscription(ctx, uuid.NewString()), openrails.ErrDenied)
	_, err = reader.DeletePaymentMethod(ctx, uuid.NewString(), uuid.NewString())
	require.ErrorIs(t, err, openrails.ErrDenied)
}
