//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/testauth"
)

// dtoShapeObservation is the id and currency spelling one deployment returned.
type dtoShapeObservation struct {
	SubscriptionID, ListedSubscriptionID, MethodSubscriptionID openrails.SubscriptionID
	CustomerID                                                 openrails.CustomerID
	ProductID, PriceProductID, ProductOwnID                    openrails.ProductID
	PriceID, PriceOwnID, CatalogPriceID                        openrails.PriceID
	PaymentMethodID, MethodID                                  openrails.PaymentMethodID
	PaymentID                                                  openrails.PaymentID
	ReadByID, HasPayment                                       bool
	DepositCurrency, BalanceCurrency, PriceCurrency            string
	RuleCurrency                                               string
}

// TestDTOShapesAreCanonicalAcrossDeployments pins the id and currency rules
// of the shared Client: every id is its typed kind (a subscription's
// payment_method_id is the same PaymentMethodID PaymentMethod.id carries, the
// catalog price id is the price the subscription names), reads accept the
// typed id, and currency codes come back in the registry's uppercase spelling
// — identically through the embedded and standalone Clients. On the wire the
// standalone surface spells each typed id with its prefix and never as a
// bare UUID.
func TestDTOShapesAreCanonicalAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("usd")
	runtime, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	local, err := runtime.Client(openrails.WithMerchantID(dbtest.TestMerchantID), openrails.WithCurrency("usd"))
	require.NoError(t, err)
	mid := dbtest.TestMerchantID.UUID()

	type fixture struct {
		customer     openrails.CustomerID
		subscription openrails.SubscriptionID
		method       openrails.PaymentMethodID
		payment      openrails.PaymentID
		product      *openrails.CatalogProduct
		price        *openrails.CatalogPrice
	}
	fixtures := map[string]fixture{}
	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": standalone.Client()} {
		key := "shape-" + uuid.NewString()[:8]
		product, err := client.CreateProduct(ctx, openrails.CreateProductRequest{Key: key, DisplayName: "Shape"})
		require.NoError(t, err)
		duration := 720
		price, err := client.CreatePrice(ctx, openrails.CreatePriceRequest{ProductID: product.ID, Key: key + "-monthly", UnitAmount: 1_000_000, Currency: "usd", AccessDurationHours: &duration, AutoRenew: true})
		require.NoError(t, err)
		require.Equal(t, product.ID, price.ProductID)

		f := fixture{customer: openrails.CustomerID(uuid.New()), subscription: openrails.SubscriptionID(uuid.New()), method: openrails.PaymentMethodID(uuid.New()), payment: openrails.PaymentID(uuid.New()), product: product, price: price}
		psp := dbtest.EnsureTestPSP(ctx, t, h.Pool(), mid, "nmi")
		exec := func(sql string, args ...any) {
			_, err := h.Pool().Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		now := time.Now().UTC()
		exec(`INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, mid, f.customer.UUID())
		exec(`INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,psp_id,rail,rail_customer_ref,rail_method_ref,initial_transaction_id,last_four,card_type) VALUES($1,$2,$3,$4,'nmi',$5::text,$5::text,$5::text,'4242','visa')`,
			f.method.UUID(), mid, f.customer.UUID(), psp, "shape-"+f.method.UUID().String())
		exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,payment_method_id,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,'nmi','active',$7,$8,$9,$10)`,
			f.subscription.UUID(), mid, f.customer.UUID(), product.ID.UUID(), price.ID.UUID(), psp, f.subscription.UUID().String(), f.method.UUID(), now, now.Add(720*time.Hour))
		exec(`INSERT INTO openrails.payments(id,merchant_id,customer_id,price_id,subscription_id,psp_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,purchased_at) VALUES($1,$2,$3,$4,$5,$6,'nmi',$7,1000000,1000000,'USD','completed','rail',$8)`,
			f.payment.UUID(), mid, f.customer.UUID(), price.ID.UUID(), f.subscription.UUID(), psp, "shape-txn-"+f.payment.UUID().String(), now)

		var o dtoShapeObservation
		sub, err := client.GetSubscription(ctx, f.subscription)
		require.NoError(t, err)
		o.SubscriptionID, o.CustomerID, o.ProductID, o.PriceID = sub.ID, sub.CustomerID, sub.ProductID, sub.PriceID
		require.NotNil(t, sub.PaymentMethodID)
		o.PaymentMethodID = *sub.PaymentMethodID
		require.Len(t, sub.Payments, 1)
		o.PaymentID = sub.Payments[0].ID
		require.NotNil(t, sub.Price)
		require.NotNil(t, sub.Product)
		o.PriceOwnID, o.PriceProductID, o.ProductOwnID = sub.Price.ID, sub.Price.ProductID, sub.Product.ID
		o.ReadByID = sub.ID == f.subscription
		listed, err := client.ListSubscriptions(ctx, openrails.SubscriptionFilter{CustomerID: f.customer})
		require.NoError(t, err)
		require.Len(t, listed.Data, 1)
		o.ListedSubscriptionID = listed.Data[0].ID
		methods, err := client.ListPaymentMethods(ctx, f.customer, openrails.PageOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, methods.Data, 1)
		o.MethodID = methods.Data[0].ID
		require.Len(t, methods.Data[0].Subscriptions, 1)
		o.MethodSubscriptionID = methods.Data[0].Subscriptions[0].ID
		catalogPrice, err := client.GetPrice(ctx, sub.PriceID)
		require.NoError(t, err)
		o.CatalogPriceID = catalogPrice.ID
		o.HasPayment, err = client.HasSettledPayment(ctx, f.customer, sub.PriceID)
		require.NoError(t, err)
		payments, err := client.ListPayments(ctx, openrails.PaymentFilter{CustomerID: f.customer, PriceID: price.ID})
		require.NoError(t, err)
		require.Len(t, payments.Data, 1)
		require.Equal(t, f.payment, payments.Data[0].ID)
		require.Equal(t, f.customer, payments.Data[0].CustomerID)
		require.NotNil(t, payments.Data[0].SubscriptionID)
		require.Equal(t, f.subscription, *payments.Data[0].SubscriptionID)
		payment, err := client.GetPayment(ctx, f.payment)
		require.NoError(t, err)
		require.Equal(t, price.ID, payment.Price.ID)

		deposit, err := client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &f.customer, Invoker: "shape", Currency: "usd", Amount: 1_000, Source: "shape", SourceID: uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, f.customer, deposit.CustomerID)
		o.DepositCurrency = deposit.Currency
		balance, err := client.Balance(ctx, f.customer)
		require.NoError(t, err)
		require.Equal(t, f.customer, balance.CustomerID)
		o.BalanceCurrency = balance.Currency
		o.PriceCurrency = price.Currency
		rules := []openrails.CheckoutRoutingRule{{Match: openrails.CheckoutRoutingMatch{Currency: "usd"}, Prefer: []string{"nmi"}}, {Prefer: []string{"nmi"}}}
		require.NoError(t, client.SetMerchantSettings(ctx, openrails.MerchantSettings{CheckoutRouting: &rules}))
		settings, err := client.GetMerchantSettings(ctx)
		require.NoError(t, err)
		require.NotNil(t, settings.CheckoutRouting)
		require.Len(t, *settings.CheckoutRouting, 2)
		o.RuleCurrency = (*settings.CheckoutRouting)[0].Match.Currency

		want := dtoShapeObservation{
			SubscriptionID: f.subscription, ListedSubscriptionID: f.subscription, MethodSubscriptionID: f.subscription,
			CustomerID: f.customer,
			ProductID:  product.ID, PriceProductID: product.ID, ProductOwnID: product.ID,
			PriceID: price.ID, PriceOwnID: price.ID, CatalogPriceID: price.ID,
			PaymentMethodID: f.method, MethodID: f.method, PaymentID: f.payment,
			ReadByID: true, HasPayment: true,
			DepositCurrency: "USD", BalanceCurrency: "USD", PriceCurrency: "USD", RuleCurrency: "USD",
		}
		require.Equal(t, want, o, "%s DTO shapes", name)
		fixtures[name] = f
	}
	require.Len(t, fixtures, 2)

	// The standalone wire spells every typed id with its prefix; the bare
	// UUID never appears as an id value.
	f := fixtures["standalone"]
	raw := getRawJSON(t, standalone.BaseURL+"/v1/merchant/subscriptions/"+openrails.SubscriptionID(f.subscription).String(), standalone.Token)
	require.Equal(t, f.subscription.String(), raw["id"])
	require.Equal(t, f.customer.String(), raw["customer_id"])
	require.Equal(t, f.product.ID.String(), raw["product_id"])
	require.Equal(t, f.price.ID.String(), raw["price_id"])
	require.Equal(t, f.method.String(), raw["payment_method_id"])
	require.Equal(t, f.payment.String(), raw["payments"].([]any)[0].(map[string]any)["id"])
	require.True(t, strings.HasPrefix(raw["id"].(string), openrails.SubscriptionIDPrefix))
	for _, bare := range []string{f.subscription.UUID().String(), f.product.ID.UUID().String(), f.price.ID.UUID().String(), f.method.UUID().String(), f.payment.UUID().String()} {
		for key, value := range raw {
			if s, ok := value.(string); ok && key != "rail_subscription_id" {
				require.NotEqual(t, bare, s, "bare uuid leaked at %s", key)
			}
		}
	}
	catalog := getRawJSON(t, standalone.BaseURL+"/v1/merchant/catalog/prices/"+openrails.PriceID(f.price.ID).String(), standalone.Token)
	require.Equal(t, f.price.ID.String(), catalog["id"])
	require.Equal(t, f.product.ID.String(), catalog["product_id"])
	// A bare UUID, or another kind's prefix, is not an id of this kind.
	for _, path := range []string{
		"/v1/merchant/catalog/prices/" + f.price.ID.UUID().String(),
		"/v1/merchant/catalog/prices/" + openrails.ProductID(f.price.ID.UUID()).String(),
		"/v1/merchant/subscriptions/" + f.subscription.UUID().String(),
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, standalone.BaseURL+path, nil)
		require.NoError(t, err)
		require.NoError(t, testauth.Authorize(req, standalone.Token))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, path)
	}
}

func getRawJSON(t *testing.T, url, token string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	require.NoError(t, testauth.Authorize(req, token))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}
