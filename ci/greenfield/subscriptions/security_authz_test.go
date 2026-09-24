//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	auth "github.com/open-rails/helpers/auth"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// refused is a customer or merchant request the engine must not honor: it
// neither succeeds nor reveals whether the target exists.
func refused(t *testing.T, status int, body any, what string) {
	t.Helper()
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, status, "%s must be refused: %d %v", what, status, body)
}

// SEC: customer IDOR. Every /v1/me route that names an object by id must
// bind it to the signed-in customer. Mallory holds valid customer credentials
// and knows Alice's ids; nothing she sends may read or change Alice's billing,
// or make Alice's card pay for Mallory.
func TestSecurityCustomerCannotActOnAnotherCustomer(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			price := w.membership("content:members", 9_990_000)
			alice, mallory := w.newCustomer(), w.newCustomer()
			aliceCard := alice.saveCard(rail, visa)
			aliceSub := alice.subscribe(embedded, rail, price.ID, "content:members", aliceCard)
			malloryCard := mallory.saveCard(rail, mastercard)
			mallorySub := mallory.subscribe(embedded, rail, price.ID, "content:members", malloryCard)
			other := w.membership("content:other", 4_990_000)
			pending, err := w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
				OfferKind: openrails.OfferRecurring, Customer: openrails.CheckoutCustomerIdentity{ID: alice.id}, Entitlement: "content:other", PriceID: other.ID,
				IdempotencyKey: "alice-pending-" + uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp[rail], Rail: rail, PaymentMethodID: aliceCard},
				SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
			})
			require.NoError(t, err)
			charges := len(w.railLedger(rail))

			for _, tc := range []struct {
				method, path string
				body         any
			}{
				{http.MethodGet, "/subscriptions/" + aliceSub.String(), nil},
				{http.MethodPost, "/subscriptions/" + aliceSub.String() + "/cancel", map[string]any{"feedback": "x"}},
				{http.MethodPost, "/subscriptions/" + aliceSub.String() + "/cancel", map[string]any{"immediately": true}},
				{http.MethodPost, "/subscriptions/" + aliceSub.String() + "/resume", nil},
				{http.MethodPost, "/subscriptions/" + aliceSub.String() + "/retry-now", nil},
				{http.MethodPut, "/subscriptions/" + aliceSub.String() + "/payment-method", map[string]any{"payment_method_id": malloryCard}},
				{http.MethodPut, "/subscriptions/" + mallorySub.String() + "/payment-method", map[string]any{"payment_method_id": aliceCard}},
				{http.MethodPut, "/collection-payment-method", map[string]any{"payment_method_id": aliceCard, "currency": "USD"}},
				{http.MethodPut, "/payment-methods/" + aliceCard, map[string]any{"provider": rail, "name_on_card": "Mallory"}},
				{http.MethodDelete, "/payment-methods/" + aliceCard, nil},
				{http.MethodGet, "/checkout/" + pending.ID, nil},
				{http.MethodPost, "/checkout/" + pending.ID + "/confirm", map[string]any{"payment": map[string]string{"rail": rail}}},
				{http.MethodPost, "/checkout/" + pending.ID + "/confirm", map[string]any{"payment": map[string]string{"rail": rail, "payment_method_id": aliceCard}}},
			} {
				status, body := mallory.call(tc.method, tc.path, "", tc.body)
				refused(t, status, body, fmt.Sprintf("%s %s %v", tc.method, tc.path, tc.body))
			}
			w.settle()

			// Mallory's own reads never include Alice's rows.
			for _, path := range []string{"/subscriptions", "/payments", "/payment-methods"} {
				_, body := mallory.call(http.MethodGet, path, "", nil)
				require.NotContains(t, fmt.Sprint(body), aliceSub.String(), path)
				require.NotContains(t, fmt.Sprint(body), aliceCard, path)
			}

			sub := w.subscription(embedded, aliceSub)
			require.Equal(t, "active", sub.Status)
			require.False(t, sub.CancelScheduled)
			require.Nil(t, sub.CancelledAt)
			require.True(t, alice.entitled("content:members"))
			require.False(t, mallory.entitled("content:other"))
			require.Len(t, w.railLedger(rail), charges, "no request charged anyone")
			require.Equal(t, "pending", unwrap(alice.must(http.MethodGet, "/checkout/"+pending.ID, "", nil))["status"])

			// Each renewal is paid by its own member's card.
			w.advance(w.subscription(embedded, mallorySub).CurrentPeriodEndsAt.Sub(w.clock.Now()) + 1)
			w.runRenewals()
			byCard := map[string]int{}
			for _, entry := range w.railLedger(rail) {
				byCard[lastFour(w, rail, entry)]++
			}
			require.Equal(t, map[string]int{visa.Last4: 2, mastercard.Last4: 2}, byCard)
		})
	}
}

func lastFour(w *world, rail string, entry ledgerEntry) string {
	if rail == "stripe" {
		return w.stripe.cardOf(entry.Method)
	}
	return entry.Method
}

// rival is a second merchant sharing the first merchant's database, as a
// multi-merchant host runs them. Its staff are authorized only for itself.
type rival struct {
	slug   string
	rt     *embed.Runtime
	server *httptest.Server
	client *openrails.Client
}

func (w *world) rival() *rival {
	return w.peer("rival-"+uuid.NewString()[:8], embed.CustomerBillingManagement, map[string]embed.PSPConfig{
		"stripe": {"stripe": {AccountID: "acct_rival", Secrets: map[string]string{"secret_key": "sk_test_rival", "webhook_signing_secret": "whsec_rival"}}},
		"nmi":    {"nmi": {AccountID: "rival-nmi", Secrets: map[string]string{"security_key": "rival-nmi-key", "webhook_signing_secret": "nmi_webhook_rival"}, Settings: map[string]any{"tokenization_key": "rival-tokenization"}}},
	})
}

// replica is another process of the same merchant on the same database, as
// hosts run several replicas behind one load balancer.
func (w *world) replica() *rival {
	return w.replicaWith(embed.CustomerBillingManagement)
}

// replicaWith is a replica publishing the given customer route scope.
func (w *world) replicaWith(scope embed.CustomerHTTPScope) *rival {
	return w.peer(w.slug, scope, w.declaredPSPs())
}

func (w *world) declaredPSPs() map[string]embed.PSPConfig {
	return map[string]embed.PSPConfig{
		"stripe": {"stripe": {AccountID: stripeAcct, Secrets: map[string]string{"secret_key": "sk_test_greenfield", "webhook_signing_secret": whsecStripe}}},
		"nmi":    {"nmi": {AccountID: nmiAcct, Secrets: map[string]string{"security_key": "greenfield-nmi-key", "webhook_signing_secret": whsecNMI}, Settings: map[string]any{"tokenization_key": "greenfield-tokenization"}}},
		"ccbill": {"ccbill": {AccountID: ccbillAcct}},
	}
}

// peer is another process on this database. Subjects "auto-<uuid>" are the
// customer's automation credentials, not their interactive session.
func (w *world) peer(slug string, scope embed.CustomerHTTPScope, psps map[string]embed.PSPConfig) *rival {
	t := w.t
	identity, err := billingauth.NewIntegration(billingauth.IntegrationOptions{
		Verifier: w.auth,
		Customer: func(_ context.Context, p auth.Principal) (billingauth.CustomerIdentity, error) {
			subject := p.Identity().Subject
			if _, err := uuid.Parse(subject); err == nil {
				return billingauth.CustomerIdentity{ID: subject, CredentialClass: billingauth.CredentialClassUserSession}, nil
			}
			if id, ok := strings.CutPrefix(subject, "auto-"); ok {
				if _, err := uuid.Parse(id); err == nil {
					return billingauth.CustomerIdentity{ID: id, CredentialClass: billingauth.CredentialClassAutomation}, nil
				}
			}
			return billingauth.CustomerIdentity{}, nil
		},
		Authority: func(_ context.Context, q billingauth.Requirement) (billingauth.Authority, error) {
			if q.Scope != billingauth.MerchantScope || q.Target.MerchantSlug != slug {
				return billingauth.Authority{}, nil
			}
			return billingauth.Authority{Scope: auth.Scope{Authority: issuer, ID: "rival-staff"}, Permission: q.Permission}, nil
		},
	})
	require.NoError(t, err)
	rt, err := embed.New(t.Context(), embed.Options{
		Auth:     identity,
		HTTP:     &embed.HTTPConfig{MerchantAdmin: true, MerchantAPI: true, Catalog: true, CustomerRoutes: []embed.CustomerRoutesConfig{{Merchant: slug, Scope: scope}}},
		Merchant: &embed.MerchantDeclaration{Slug: slug, Config: embed.MerchantConfig{DisplayName: slug, PSPs: psps}},
		Config: &config.Config{
			TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull, AllowCatalogUpdates: true,
			DB: &config.DBConfig{URL: w.dsn, Schema: w.schema}, TrustedProxies: []string{"127.0.0.1/32"},
		},
		PGXPool: w.pool, River: embed.RiverFromHost(), StripeTransport: w.stripe, NMITransport: w.nmi, Clock: w.clock,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	bundle, err := openrailshttp.Routes(rt)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, bundle.Mount(mux, mountPrefix))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := rt.Client()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return rt.Ready(t.Context()) == nil }, 10*time.Second, 50*time.Millisecond, "peer readiness")
	return &rival{slug: slug, rt: rt, server: server, client: client}
}

// SEC: merchant isolation. A second merchant on the same database, and staff
// authorized for one merchant, cannot read or act on the other's objects.
func TestSecurityMerchantIsolation(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	payment := completed(w.payments(embedded, e.c.id))[0]
	r := w.rival()
	ctx := t.Context()

	// Merchant B's own Client, by merchant A's typed ids.
	_, err := r.client.GetSubscription(ctx, e.sub)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = r.client.GetPayment(ctx, payment.ID)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = r.client.RefundPayment(ctx, payment.ID, openrails.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "rival-refund"})
	require.Error(t, err)
	require.Error(t, r.client.CancelSubscription(ctx, e.sub, openrails.CancelSubscriptionRequest{}))
	price, err := w.client[embedded].Prices.Retrieve(ctx, e.price)
	require.NoError(t, err)
	_, err = r.client.Prices.Retrieve(ctx, price.ID)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = r.client.Products.Retrieve(ctx, price.ProductID)
	require.ErrorIs(t, err, openrails.ErrNotFound)
	list, err := r.client.ListPayments(ctx, openrails.PaymentFilter{CustomerID: e.c.id})
	require.NoError(t, err)
	require.Empty(t, list.Data)

	// Merchant B cannot sell merchant A's price, or charge merchant A's saved card.
	_, err = r.client.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{
		OfferKind: openrails.OfferRecurring, Customer: openrails.CheckoutCustomerIdentity{ID: e.c.id}, Entitlement: e.ent, PriceID: e.price,
		IdempotencyKey: "rival-price-" + uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "nmi", PaymentMethodID: e.method},
		SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return",
	})
	require.Error(t, err)
	own := r.client
	product, err := own.Products.Create(ctx, &openrails.ProductCreateParams{Key: "rival-" + uuid.NewString()[:8], DisplayName: "Rival", EntitlementsSpec: map[string]*int{"content:rival": nil}})
	require.NoError(t, err)
	rivalPrice, err := own.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	_, err = own.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{
		OfferKind: openrails.OfferPermanent, Customer: openrails.CheckoutCustomerIdentity{ID: e.c.id}, Entitlement: "content:rival", PriceID: rivalPrice.ID,
		IdempotencyKey: "rival-card-" + uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "nmi", PaymentMethodID: e.method},
		SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return",
	})
	require.Error(t, err, "a foreign merchant's saved card is not chargeable")
	require.Len(t, e.providerLedger(), 1, "nothing charged the member's card")

	// Staff authorized for merchant A cannot select merchant B on either
	// merchant's mount; the same request for A is allowed.
	merchantGet := func(server, token, slug string) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+mountPrefix+"/v1/merchant/findings", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-OpenRails-Merchant-Slug", slug)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		res.Body.Close()
		return res.StatusCode
	}
	staff := w.auth.token(t, "staff")
	require.Equal(t, http.StatusOK, merchantGet(w.server.URL, staff, w.slug))
	for _, server := range []string{w.server.URL, r.server.URL} {
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}, merchantGet(server, staff, r.slug))
	}
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, merchantGet(w.server.URL, e.c.token, w.slug), "a customer credential is not merchant authority")
	crossed, err := openrails.NewRemote(w.server.URL+mountPrefix, openrails.WithDefaultMerchant(r.slug),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return w.auth.token(t, "staff"), nil }))
	require.NoError(t, err)
	_, err = crossed.GetSubscription(ctx, e.sub)
	require.Error(t, err)
	_, err = crossed.RefundPayment(ctx, payment.ID, openrails.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "crossed-refund"})
	require.Error(t, err)

	// A customer credential is never merchant authority.
	customerRemote, err := openrails.NewRemote(w.server.URL+mountPrefix, openrails.WithDefaultMerchant(w.slug),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return e.c.token, nil }))
	require.NoError(t, err)
	_, err = customerRemote.RefundPayment(ctx, payment.ID, openrails.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "customer-refund"})
	require.Error(t, err)
	_, err = customerRemote.ListPayments(ctx, openrails.PaymentFilter{})
	require.Error(t, err)

	w.settle()
	got, err := w.client[embedded].GetPayment(ctx, payment.ID)
	require.NoError(t, err)
	require.False(t, got.Refunded)
	require.Equal(t, "active", w.subscription(embedded, e.sub).Status)
	for _, entry := range e.providerLedger() {
		require.Zero(t, entry.Refunded)
	}
}
