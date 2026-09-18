//go:build integration

package billingapp_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/examples/billingapp"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMain(m *testing.M) {
	time.Local = time.UTC
	dbtest.RunMain(m)
}

func ccbill(account string) config.PSPSet {
	return config.PSPSet{"ccbill": {AccountID: account, CCBill: &config.CCBillRailConfig{Salt: "billingapp-local-fixture"}}}
}

// No provider endpoint is called: CCBill checkout signs a redirect locally,
// cancellation/resume enqueue durable intents, and the invoice receivable is
// closed by the same money service the invoice worker uses.
func TestBillingApplicationRunsUnchangedAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("USD", integrationharness.WithRails(ccbill("999981-0000")))
	tenant := standalone.ProvisionOwnedMerchant("billingapp-tenant")
	integrationharness.SeedPSPs(ctx, t, standalone.App().Runtime, tenant.MerchantID, ccbill("999981-0001"))

	newRuntime := func() *embed.Runtime {
		rt, err := embed.New(ctx, embed.Options{
			Config: &config.Config{
				Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI,
				SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull,
				DB: &config.DBConfig{URL: h.DSN},
			},
			Redis: h.Redis, River: embed.RiverManagedByOpenRails(),
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
		return rt
	}
	bound := newRuntime()
	// An AuthKit-free embedded host owns an unbound merchant identity.
	embeddedMerchant, err := bound.UpsertMerchantConfig(ctx, "billingapp-embedded", embed.MerchantConfig{})
	require.NoError(t, err)
	integrationharness.SeedPSPs(ctx, t, standalone.App().Runtime, embeddedMerchant, ccbill("999981-0002"))
	embeddedClient, err := bound.Client(openrails.WithCurrency("USD"))
	require.NoError(t, err)
	unbound := newRuntime()
	sharedEngineClient, err := unbound.Client(openrails.WithMerchantID(dbtest.TestMerchantID), openrails.WithCurrency("USD"))
	require.NoError(t, err)

	deployments := []struct {
		name     string
		merchant merchant.ID
		client   *openrails.Client
	}{
		{"embedded", embeddedMerchant, embeddedClient},
		{"standalone_http", dbtest.TestMerchantID, standalone.Client()},
		{"shared_engine", dbtest.TestMerchantID, sharedEngineClient},
		{"hosted_tenant_http", tenant.MerchantID, standalone.Client(
			openrails.WithTokenProvider(func(context.Context) (string, error) { return tenant.APIKey, nil }),
			openrails.WithMerchantID(tenant.MerchantID),
		)},
	}
	var want *billingapp.Report
	for _, deployment := range deployments {
		t.Run(deployment.name, func(t *testing.T) {
			in, price := seedMerchantFacts(ctx, t, h, standalone, deployment.merchant)
			got, err := billingapp.Run(ctx, deployment.client, in)
			require.NoError(t, err)
			require.Equal(t, billingapp.Report{
				PolicyWindows: 1, Deposited: 100_000, Admitted: true, Captured: 7_500, DeniedBy: "money",
				UnknownRelease: true, Balance: 92_500, UsageEvents: 1, CheckoutRails: 1, CheckoutReplayed: true,
				CheckoutAmount: 10_000_000, SubscriptionStatus: "cancelled", CancelScheduled: true, Resumed: true,
				InvoiceProfileSet: true, InvoiceDueAfterPaid: 400, InvoiceStatus: "voided",
			}, got)
			assertDurableFacts(ctx, t, h, deployment.merchant, in, price)
			if want == nil {
				want = &got
				return
			}
			require.Equal(t, *want, got, "deployment changed application-visible behavior")
		})
	}
}

func seedMerchantFacts(ctx context.Context, t *testing.T, h *integrationharness.Harness, standalone *integrationharness.Surface, mid merchant.ID) (billingapp.Inputs, uuid.UUID) {
	t.Helper()
	pool := h.Pool()
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	run := uuid.NewString()
	product, price := uuid.New(), uuid.New()
	priceKey := "billingapp-" + price.String()
	exec(`INSERT INTO openrails.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Membership')`, product, mid.UUID(), "billingapp-"+product.String())
	exec(`INSERT INTO openrails.prices(id,merchant_id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,$4,10000000,'USD',720,true)`, price, mid.UUID(), product, priceKey)
	ccbillPSP := dbtest.EnsureTestPSP(ctx, t, pool, mid.UUID(), "ccbill")
	exec(`INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,flex_id,configuration) VALUES($1,$2,$3,$4,'{"form_name":"billingapp-form"}')`, mid.UUID(), price, ccbillPSP, uuid.NewString())

	subscriber, method, subscription, nmiPSP := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	start := time.Now().UTC()
	exec(`INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, mid.UUID(), subscriber)
	exec(`INSERT INTO openrails.psps(id,merchant_id,rail,environment,account_id,key) VALUES($1,$2,'nmi','test',$3,$3)`, nmiPSP, mid.UUID(), nmiPSP.String())
	exec(`INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,psp_id,rail,initial_transaction_id,last_four,card_type) VALUES($1,$2,$3,$4,'nmi','billingapp-anchor','4242','visa')`, method, mid.UUID(), subscriber, nmiPSP)
	exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,payment_method_id,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,'nmi','active',$7,$8,$9,$10)`,
		subscription, mid.UUID(), subscriber, product, price, nmiPSP, subscription.String(), method, start, start.Add(48*time.Hour))

	debtor := identity.CustomerID(uuid.New())
	rt := standalone.App().Runtime
	scoped := merchant.WithID(ctx, mid)
	mode := money.BillingModeArrears
	var invoiceID uuid.UUID
	require.NoError(t, rt.DB.RunInMerchantConn(scoped, func(c context.Context) error {
		if _, err := rt.MoneyService.UpsertAccountSettings(c, debtor, "USD", money.AccountSettingsInput{BillingMode: &mode}); err != nil {
			return err
		}
		if _, err := rt.MoneyService.AccrueOwed(c, debtor, "USD", "billingapp", run, 800); err != nil {
			return err
		}
		invoice, err := rt.MoneyService.FinalizeInvoice(c, debtor, "USD", time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
		invoiceID = invoice.ID
		return err
	}))
	return billingapp.Inputs{
		Currency: "USD", Run: run, CheckoutPriceKey: priceKey, CheckoutRail: "ccbill",
		SubscriberID: openrails.CustomerID(subscriber), SubscriptionID: openrails.SubscriptionID(subscription), InvoiceID: invoiceID,
	}, price
}

// The application's receipts are backed by committed merchant-scoped facts.
func assertDurableFacts(ctx context.Context, t *testing.T, h *integrationharness.Harness, mid merchant.ID, in billingapp.Inputs, price uuid.UUID) {
	t.Helper()
	pool := h.Pool()
	var intents, sessions int
	var status string
	subscription, err := api.ParseSubscriptionID(in.SubscriptionID)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.rail_intents WHERE merchant_id=$1 AND subscription_id=$2`, mid.UUID(), subscription).Scan(&intents))
	require.Positive(t, intents, "cancellation is a durable provider intent")
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.checkout_sessions WHERE merchant_id=$1 AND price_id=$2`, mid.UUID(), price).Scan(&sessions))
	require.Equal(t, 1, sessions, "checkout replay creates one session")
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM openrails.invoices WHERE merchant_id=$1 AND id=$2`, mid.UUID(), in.InvoiceID).Scan(&status))
	require.Equal(t, "voided", status)
}
