//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/embedded"
)

// dtoShapeObservation is the id and currency spelling one deployment returned.
type dtoShapeObservation struct {
	SubscriptionID, ProductID, PriceID, PaymentMethodID, PaymentID string
	PriceOwnID, PriceProductID, ProductOwnID                       string
	ListedSubscriptionID, MethodID, MethodSubscriptionID           string
	ReadByPrefixedID                                               bool
	DepositCurrency, BalanceCurrency, PriceCurrency, RuleCurrency  string
}

// TestDTOShapesAreCanonicalAcrossDeployments pins two wire rules on the shared
// Client: ids of prefixed resource kinds travel prefixed on every DTO (a
// subscription's payment_method_id is the same pm_ string PaymentMethod.id
// carries), and currency codes come back in the registry's uppercase spelling
// whatever case the caller sent — identically through the embedded and
// standalone Clients.
func TestDTOShapesAreCanonicalAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("usd")
	runtime, err := embed.New(ctx, embed.Options{Options: embedded.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embedded.RiverManagedByOpenRails(),
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	local, err := runtime.Client(openrails.WithMerchantID(dbtest.TestMerchantID), openrails.WithCurrency("usd"))
	require.NoError(t, err)
	mid := dbtest.TestMerchantID.UUID()

	observed := map[string]dtoShapeObservation{}
	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": standalone.Client()} {
		key := "shape-" + uuid.NewString()[:8]
		product, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: key, DisplayName: "Shape"})
		require.NoError(t, err)
		duration := 720
		price, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, Key: key + "-monthly", UnitAmount: 1_000_000, Currency: "usd", AccessDurationHours: &duration, AutoRenew: true})
		require.NoError(t, err)

		customer, subscription, method, payment := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		psp := dbtest.EnsureTestPSP(ctx, t, h.Pool(), mid, "nmi")
		exec := func(sql string, args ...any) {
			_, err := h.Pool().Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		now := time.Now().UTC()
		exec(`INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
		exec(`INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,psp_id,rail,rail_customer_ref,rail_method_ref,initial_transaction_id,last_four,card_type) VALUES($1,$2,$3,$4,'nmi',$5::text,$5::text,$5::text,'4242','visa')`,
			method, mid, customer, psp, "shape-"+method.String())
		exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,payment_method_id,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,'nmi','active',$7,$8,$9,$10)`,
			subscription, mid, customer, product.ID, price.ID, psp, subscription.String(), method, now, now.Add(720*time.Hour))
		exec(`INSERT INTO openrails.payments(id,merchant_id,customer_id,price_id,subscription_id,psp_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,purchased_at) VALUES($1,$2,$3,$4,$5,$6,'nmi',$7,1000000,1000000,'USD','completed','none',$8)`,
			payment, mid, customer, price.ID, subscription, psp, "shape-txn-"+payment.String(), now)

		var o dtoShapeObservation
		sub, err := client.GetSubscription(ctx, subscription.String())
		require.NoError(t, err)
		o.SubscriptionID, o.ProductID, o.PriceID = sub.ID, sub.ProductID, sub.PriceID
		require.NotNil(t, sub.PaymentMethodID)
		o.PaymentMethodID = *sub.PaymentMethodID
		require.Len(t, sub.Payments, 1)
		o.PaymentID = sub.Payments[0].ID
		require.NotNil(t, sub.Price)
		require.NotNil(t, sub.Product)
		o.PriceOwnID, o.PriceProductID, o.ProductOwnID = sub.Price.ID, sub.Price.ProductID, sub.Product.ID
		byPrefixed, err := client.GetSubscription(ctx, sub.ID)
		require.NoError(t, err)
		o.ReadByPrefixedID = byPrefixed.ID == sub.ID
		listed, err := client.ListSubscriptions(ctx, openrails.SubscriptionFilter{CustomerID: customer.String()})
		require.NoError(t, err)
		require.Len(t, listed.Data, 1)
		o.ListedSubscriptionID = listed.Data[0].ID
		methods, err := client.ListPaymentMethods(ctx, customer.String(), openrails.PageOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, methods.Data, 1)
		o.MethodID = methods.Data[0].ID
		require.Len(t, methods.Data[0].Subscriptions, 1)
		o.MethodSubscriptionID = methods.Data[0].Subscriptions[0].ID

		payer := openrails.CustomerID(customer)
		deposit, err := client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "shape", Currency: "usd", Amount: 1_000, Source: "shape", SourceID: uuid.NewString()})
		require.NoError(t, err)
		o.DepositCurrency = deposit.Currency
		balance, err := client.Balance(ctx, customer.String())
		require.NoError(t, err)
		o.BalanceCurrency = balance.Currency
		o.PriceCurrency = price.Currency
		rules := []openrails.CheckoutRoutingRule{{Match: openrails.CheckoutRoutingMatch{Currency: "usd"}, Prefer: []string{"nmi"}}, {Prefer: []string{"nmi"}}}
		require.NoError(t, client.SetMerchantSettings(ctx, openrails.MerchantSettings{CheckoutRouting: &rules}))
		settings, err := client.GetMerchantSettings(ctx)
		require.NoError(t, err)
		require.NotNil(t, settings.CheckoutRouting)
		require.Len(t, *settings.CheckoutRouting, 2)
		o.RuleCurrency = (*settings.CheckoutRouting)[0].Match.Currency

		// Normalize the per-deployment fixture ids to their expected prefixed
		// spellings so the two observations compare equal.
		want := dtoShapeObservation{
			SubscriptionID: api.FormatSubscriptionID(subscription), ProductID: api.FormatProductID(product.ID), PriceID: api.FormatPriceID(price.ID),
			PaymentMethodID: api.FormatPaymentMethodID(method), PaymentID: api.FormatPaymentID(payment),
			PriceOwnID: api.FormatPriceID(price.ID), PriceProductID: api.FormatProductID(product.ID), ProductOwnID: api.FormatProductID(product.ID),
			ListedSubscriptionID: api.FormatSubscriptionID(subscription), MethodID: api.FormatPaymentMethodID(method), MethodSubscriptionID: api.FormatSubscriptionID(subscription),
			ReadByPrefixedID: true,
			DepositCurrency:  "USD", BalanceCurrency: "USD", PriceCurrency: "USD", RuleCurrency: "USD",
		}
		require.Equal(t, want, o, "%s DTO shapes", name)
		observed[name] = o
	}
	require.Len(t, observed, 2)
}
