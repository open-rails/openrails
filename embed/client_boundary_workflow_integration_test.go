//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/testauth"
	"github.com/open-rails/openrails/permissions"
)

func TestClientBoundaryWorkflow(t *testing.T) {
	require.ErrorIs(t, billingservice.ErrIdempotencyKeyReused, money.ErrIdempotencyKeyReused)
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	var reference map[string]errorObservation
	for _, d := range clientWorkflowDeployments(t, h) {
		t.Run(d.name, func(t *testing.T) {
			client := d.client
			checkClientDTOShapes(t, ctx, h, d)
			checkClientCredentials(t, ctx, d)
			valid := openrails.CustomerID(uuid.New())
			for _, tc := range []struct {
				name   string
				server bool
				call   func() error
			}{
				{"zero subscription", false, func() error { _, err := client.GetSubscription(ctx, openrails.SubscriptionID{}); return err }},
				{"blank grant", false, func() error { return client.RevokeEntitlement(ctx, valid, "") }},
				{"zero customer grant", false, func() error {
					_, err := client.GrantEntitlement(ctx, openrails.CustomerID{}, openrails.GrantEntitlementRequest{Entitlement: "pro"})
					return err
				}},
				{"blank migration source", false, func() error {
					_, err := client.PreviewPlanMigration(ctx, openrails.PlanMigrationRequest{SourcePrice: " ", TargetPrice: valid.String()})
					return err
				}},
				{"nil migration batch", false, func() error { _, err := client.CancelPlanMigration(ctx, uuid.Nil); return err }},
				{"nil price key", false, func() error { _, err := client.SetPriceKey(ctx, openrails.PriceID{}, "key"); return err }},
				{"nil invoice", false, func() error { _, err := client.GetMerchantInvoice(ctx, uuid.Nil); return err }},
				{"zero balance customer", false, func() error { _, err := client.Balance(ctx, openrails.CustomerID{}); return err }},
				{"dot operation", false, func() error { _, err := client.GetOperationAuthorization(ctx, ".."); return err }},
				{"zero entitlement subject", false, func() error { _, err := client.ListEntitlements(ctx, openrails.CustomerID{}, time.Now()); return err }},
				{"malformed grant", true, func() error { return client.RevokeEntitlement(ctx, valid, "not-a-uuid") }},
				{"empty admission batch", true, func() error { _, err := client.AdmitBatch(ctx, nil); return err }},
				{"zero entitlement subjects", true, func() error {
					_, err := client.ListActiveEntitlements(ctx, []openrails.CustomerID{{}, {}}, time.Time{})
					return err
				}},
				{"too many entitlement subjects", true, func() error {
					ids := make([]openrails.CustomerID, 501)
					for i := range ids {
						ids[i] = openrails.CustomerID(uuid.New())
					}
					_, err := client.ListActiveEntitlements(ctx, ids, time.Time{})
					return err
				}},
				{"zero access product", false, func() error {
					_, err := client.ProductAccess.Check(ctx, &openrails.ProductAccessCheckParams{CustomerID: valid.String()})
					return err
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := observeClientError(t, tc.name, tc.call())
					require.Equal(t, errorObservation{StatusError: true, Status: 400, Type: "invalid_request_error", Code: "invalid_param", Invalid: true, HasRequestID: tc.server}, got)
				})
			}
			observed := clientBoundaryErrors(t, ctx, h, d)
			require.Equal(t, errorObservation{StatusError: true, Status: 400, Type: "invalid_request_error", Code: "payment_method_delete_unsupported", Metadata: map[string]any{"rail": "stripe"}, HasRequestID: true, Invalid: true}, observed["delete_unsupported_rail"])
			require.Equal(t, "customer_id", observed["admit_missing_customer"].Param)
			require.True(t, observed["usage_key_reused"].IdempotencyKeyReused)
			require.Equal(t, errorObservation{Unreachable: true, Canceled: true}, observed["canceled"])
			require.Equal(t, errorObservation{Unreachable: true, DeadlineExceeded: true}, observed["deadline"])
			if reference == nil {
				reference = observed
			} else {
				require.Equal(t, reference, observed, "all machine-visible refusal properties match")
			}

		})
	}
}

func clientBoundaryErrors(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) map[string]errorObservation {
	client := d.client
	t.Helper()
	mid := d.mid.UUID()
	customer, product, price, nmi, stripe, card, portalCard, sub := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	exec := func(sql string, args ...any) {
		_, err := h.Pool().Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	// These PSPs are unique to this deployment. Archive them after the checks;
	// subscription transition history is immutable, even to a fixture owner.
	// The process-owned scratch database is dropped by dbtest after the suite.
	t.Cleanup(func() {
		_, err := h.Pool().Exec(context.Background(), `UPDATE billing.psps SET archived=true, updated_at=now() WHERE merchant_id=$1 AND id IN ($2,$3)`, mid, nmi, stripe)
		require.NoError(t, err)
	})
	exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
	exec(`INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Parity plan')`, product, mid, product.String())
	exec(`INSERT INTO billing.prices(id,merchant_id,product_id,key,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,$4,1000000,'USD',true,720)`, price, mid, product, price.String())
	exec(`INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,key) VALUES($1,$2,'nmi','test',$3,$3),($4,$2,'stripe','test',$5,$5)`, nmi, mid, nmi.String(), stripe, stripe.String())
	exec(`INSERT INTO billing.payment_methods(id,merchant_id,customer_id,psp_id,rail,initial_transaction_id,last_four,card_type) VALUES($1,$2,$3,$4,'nmi','parity-anchor','4242','visa'),($5,$2,$3,$6,'stripe','parity-portal','4444','mastercard')`, card, mid, customer, nmi, portalCard, stripe)
	exec(`INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,payment_method_id,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,'nmi','active',$7,$8,$9,$10)`, sub, mid, customer, product, price, nmi, sub.String(), card, now, now.Add(48*time.Hour))

	out := map[string]errorObservation{}
	_, err := client.DeletePaymentMethod(ctx, openrails.CustomerID(customer), openrails.PaymentMethodID(card))
	out["delete_in_use"] = observeClientError(t, "delete in use", err)
	_, err = client.DeletePaymentMethod(ctx, openrails.CustomerID(uuid.New()), openrails.PaymentMethodID(card))
	out["delete_foreign_customer"] = observeClientError(t, "delete foreign customer", err)
	_, err = client.DeletePaymentMethod(ctx, openrails.CustomerID(customer), openrails.PaymentMethodID(portalCard))
	out["delete_unsupported_rail"] = observeClientError(t, "delete unsupported rail", err)
	_, err = client.GetSubscription(ctx, openrails.SubscriptionID(uuid.New()))
	out["subscription_not_found"] = observeClientError(t, "subscription not found", err)
	_, err = client.ListPaymentMethods(ctx, openrails.CustomerID(customer), openrails.PageOptions{Limit: 101})
	out["page_limit"] = observeClientError(t, "page limit", err)
	_, err = client.GetProduct(ctx, openrails.ProductID(uuid.New()))
	require.Equal(t, errorObservation{StatusError: true, Status: 404, Type: "invalid_request_error", Code: "product_not_found", NotFound: true, HasRequestID: true}, observeClientError(t, "product not found", err))
	err = client.CancelSubscription(ctx, openrails.SubscriptionID(uuid.New()), openrails.CancelSubscriptionRequest{Reason: "parity"})
	require.Equal(t, errorObservation{StatusError: true, Status: 404, Type: "invalid_request_error", Code: "subscription_not_found", NotFound: true, HasRequestID: true}, observeClientError(t, "subscription not found", err))
	exec(`UPDATE billing.subscriptions SET status='cancelled',cancelled_at=now(),cancel_type='merchant' WHERE id=$1`, sub)
	err = client.CancelSubscription(ctx, openrails.SubscriptionID(sub), openrails.CancelSubscriptionRequest{Reason: "parity"})
	require.Equal(t, errorObservation{StatusError: true, Status: 409, Type: "invalid_request_error", Code: "subscription_not_active", Conflict: true, HasRequestID: true}, observeClientError(t, "subscription already cancelled", err))
	_, err = client.Admit(ctx, openrails.AdmitRequest{Currency: "USD"})
	out["admit_missing_customer"] = observeClientError(t, "admit missing customer", err)

	payer := openrails.CustomerID(customer)
	_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "parity", Currency: "conformance_missing_currency", Amount: 1, Source: "parity", SourceID: uuid.NewString()})
	require.Equal(t, errorObservation{StatusError: true, Status: 400, Type: "invalid_request_error", Code: "currency_unsupported", Param: "currency", HasRequestID: true, Invalid: true}, observeClientError(t, "unsupported currency", err))
	_, err = client.UsageRollup(ctx, payer, "USD", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "bogus")
	require.Equal(t, errorObservation{StatusError: true, Status: 400, Type: "invalid_request_error", Code: "invalid_param", HasRequestID: true, Invalid: true}, observeClientError(t, "unknown rollup grouping", err))
	_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "parity", Currency: "USD", Amount: 1_000_000, Source: "parity", SourceID: uuid.NewString()})
	require.NoError(t, err)
	usage := openrails.UsageReport{CustomerID: openrails.CustomerID(customer), Invoker: "parity", Currency: "USD", EventType: "parity", Amount: 50_000, Source: "parity", SourceID: uuid.NewString()}
	require.NoError(t, client.RecordUsage(ctx, usage))
	usage.Amount = 90_000
	conflict := client.RecordUsage(ctx, usage)
	out["usage_key_reused"] = observeClientError(t, "usage key reused", conflict)
	var detail *openrails.StatusError
	require.ErrorAs(t, conflict, &detail)
	require.Equal(t, 409, detail.Status)
	require.Equal(t, "idempotency_key_reused", detail.Code)
	require.Equal(t, "invalid_request_error", detail.Type)
	require.NotEmpty(t, detail.RequestID)
	require.Contains(t, detail.Message, "90000")
	require.Contains(t, detail.Message, "50000")
	usage.Amount = 50_000
	require.NoError(t, client.RecordUsage(ctx, usage))

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = client.GetMerchantSettings(canceled)
	out["canceled"] = observeClientError(t, "canceled", err)
	expired, cancelExpired := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancelExpired()
	_, err = client.GetMerchantSettings(expired)
	out["deadline"] = observeClientError(t, "deadline", err)
	return out
}

func checkClientDTOShapes(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	client, mid, name := d.client, d.mid.UUID(), d.name
	type fixture struct {
		customer     openrails.CustomerID
		subscription openrails.SubscriptionID
		method       openrails.PaymentMethodID
		payment      openrails.PaymentID
		product      *openrails.CatalogProduct
		price        *openrails.CatalogPrice
	}
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
	now := microInstant(time.Now())
	exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid, f.customer.UUID())
	exec(`INSERT INTO billing.payment_methods(id,merchant_id,customer_id,psp_id,rail,rail_customer_ref,rail_method_ref,initial_transaction_id,last_four,card_type,created_at,updated_at) VALUES($1,$2,$3,$4,'nmi',$5::text,$5::text,$5::text,'4242','visa',$6,$6)`,
		f.method.UUID(), mid, f.customer.UUID(), psp, "shape-"+f.method.UUID().String(), now)
	exec(`INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,payment_method_id,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,'nmi','active',$7,$8,$9,$10)`,
		f.subscription.UUID(), mid, f.customer.UUID(), product.ID.UUID(), price.ID.UUID(), psp, f.subscription.UUID().String(), f.method.UUID(), now, now.Add(720*time.Hour))
	exec(`INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,subscription_id,psp_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,purchased_at) VALUES($1,$2,$3,$4,$5,$6,'nmi',$7,1000000,1000000,'USD','completed','rail',$8)`,
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
	require.True(t, methods.Data[0].CreatedAt.Equal(now))
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
	raw := getRawJSON(t, d.url+"/v1/merchant/subscriptions/"+openrails.SubscriptionID(f.subscription).String(), d.token)
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
	catalog := getRawJSON(t, d.url+"/v1/merchant/catalog/prices/"+openrails.PriceID(f.price.ID).String(), d.token)
	require.Equal(t, f.price.ID.String(), catalog["id"])
	require.Equal(t, f.product.ID.String(), catalog["product_id"])
	// A bare UUID, or another kind's prefix, is not an id of this kind.
	for _, path := range []string{
		"/v1/merchant/catalog/prices/" + f.price.ID.UUID().String(),
		"/v1/merchant/catalog/prices/" + openrails.ProductID(f.price.ID.UUID()).String(),
		"/v1/merchant/subscriptions/" + f.subscription.UUID().String(),
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url+path, nil)
		require.NoError(t, err)
		require.NoError(t, testauth.Authorize(req, d.token))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, path)
	}
}

func checkClientCredentials(t *testing.T, ctx context.Context, d clientWorkflowDeployment) {
	t.Helper()
	require.NoError(t, d.client.Verify(ctx))
	_, err := d.client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	invalid, err := openrails.NewRemote("not a url", openrails.WithAPIKey("whatever"))
	require.ErrorContains(t, err, "base URL")
	require.Nil(t, invalid)
	empty, err := openrails.NewRemote(d.url, openrails.WithAPIKey("  "))
	require.ErrorContains(t, err, "WithAPIKey")
	require.Nil(t, empty)
	unreachable, err := openrails.NewRemote("http://127.0.0.1:1", openrails.WithAPIKey("whatever"), openrails.WithTimeout(2*time.Second))
	require.NoError(t, err)
	require.ErrorIs(t, unreachable.Verify(ctx), openrails.ErrUnreachable)
	if d.name != "standalone" {
		return
	}
	bad, err := openrails.NewRemote(d.url, openrails.WithAPIKey("openrails_st_wrong_token"), openrails.WithTimeout(30*time.Second))
	require.NoError(t, err)
	require.ErrorIs(t, bad.Verify(ctx), openrails.ErrUnauthorized)
	_, err = bad.Balance(ctx, openrails.CustomerID(uuid.New()))
	require.ErrorIs(t, err, openrails.ErrUnauthorized)
	wrongBinding, err := openrails.NewRemote(d.url, openrails.WithAPIKey(d.token), openrails.WithMerchantID(openrails.MerchantID(uuid.New())))
	require.NoError(t, err)
	require.ErrorIs(t, wrongBinding.Verify(ctx), openrails.ErrConflict)
	response, err := http.Get(d.url + "/v1/currencies")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var registry openrails.CurrencyRegistry
	require.NoError(t, json.NewDecoder(response.Body).Decode(&registry))
	require.Equal(t, openrails.CurrencyRegistry{Object: "currencies", Currencies: openrails.Currencies()}, registry)
	for _, allowed := range []string{permissions.MerchantCatalogRead, permissions.MerchantCustomerSettingsRead} {
		reader, err := openrails.NewRemote(d.url, openrails.WithAPIKey(d.authority.MintAPIKey(d.slug, "reader", []string{allowed})))
		require.NoError(t, err)
		if allowed == permissions.MerchantCatalogRead {
			_, err = reader.EnsureCustomer(ctx, openrails.CustomerID(uuid.New()))
			require.ErrorIs(t, err, openrails.ErrDenied)
			_, err = reader.ImportBilling(ctx, openrails.DeclaredBilling{AsOf: time.Now()})
			require.ErrorIs(t, err, openrails.ErrDenied)
			_, err = reader.GrantEntitlement(ctx, openrails.CustomerID(uuid.New()), openrails.GrantEntitlementRequest{Entitlement: "premium"})
			require.ErrorIs(t, err, openrails.ErrDenied)
			_, err = reader.CreatePlanMigration(ctx, openrails.PlanMigrationRequest{SourcePrice: uuid.NewString(), TargetPrice: uuid.NewString()})
			require.ErrorIs(t, err, openrails.ErrDenied)
		} else {
			_, err = reader.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{Customer: openrails.CheckoutCustomerIdentity{ID: openrails.CustomerID(uuid.New())}, IdempotencyKey: uuid.NewString()})
			require.ErrorIs(t, err, openrails.ErrDenied)
		}
	}
}
