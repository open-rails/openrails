//go:build e2e && integration

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
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
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
			pending, err := w.client[embedded].CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionRequest{
				OfferKind: billing.OfferRecurring, Customer: billing.CheckoutCustomerIdentity{ID: alice.id}, Entitlement: "content:other", PriceID: other.ID,
				IdempotencyKey: "alice-pending-" + uuid.NewString(), PaymentOptions: billing.CheckoutPaymentOptions{PSPID: w.psp[rail], Rail: rail, PaymentMethodID: aliceCard},
				SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
			})
			require.NoError(t, err)
			charges := len(w.railLedger(rail))
			pendingStatus := unwrap(alice.must(http.MethodGet, "/checkout/"+pending.ID, "", nil))["status"]

			for _, tc := range []struct {
				method, path string
				body         any
			}{
				{http.MethodGet, "/subscriptions/" + aliceSub.String(), nil},
				{http.MethodPost, "/subscriptions/" + aliceSub.String() + "/cancel", map[string]any{"feedback": "no longer needed"}},
				{http.MethodPost, "/subscriptions/" + aliceSub.String() + "/resume", map[string]any{}},
				{http.MethodPost, "/subscriptions/" + aliceSub.String() + "/retry-now", map[string]any{}},
				{http.MethodPut, "/subscriptions/" + aliceSub.String() + "/payment-method", map[string]any{"payment_method_id": malloryCard}},
				{http.MethodPut, "/subscriptions/" + mallorySub.String() + "/payment-method", map[string]any{"payment_method_id": aliceCard}},

				{http.MethodDelete, "/payment-methods/" + aliceCard, nil},
				{http.MethodGet, "/checkout/" + pending.ID, nil},
				{http.MethodPost, "/checkout/" + pending.ID + "/confirm", map[string]any{"payment": map[string]string{"rail": rail}}},
			} {
				status, body := mallory.call(tc.method, tc.path, "idor-"+uuid.NewString(), tc.body)
				refused(t, status, body, fmt.Sprintf("%s %s %v", tc.method, tc.path, tc.body))
			}
			// A confirm names no payment method: Alice's card there is not read.
			status, body := mallory.call(http.MethodPost, "/checkout/"+pending.ID+"/confirm", "idor-"+uuid.NewString(), map[string]any{"payment": map[string]string{"rail": rail, "payment_method_id": aliceCard}})
			require.Contains(t, []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound}, status, "%v", body)
			// A foreign card is as ineligible as a missing one.
			status, body = mallory.call(http.MethodPut, "/collection-payment-method", "", map[string]any{"payment_method_id": aliceCard, "currency": "USD"})
			require.Contains(t, []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound}, status, "%v", body)
			_, missing := mallory.call(http.MethodPut, "/collection-payment-method", "", map[string]any{"payment_method_id": "pm_" + uuid.NewString(), "currency": "USD"})
			require.Equal(t, fmt.Sprint(missing["error"].(map[string]any)["message"]), fmt.Sprint(body["error"].(map[string]any)["message"]))
			if rail == "nmi" {
				status, body := mallory.call(http.MethodPut, "/payment-methods/"+aliceCard, "", map[string]any{"provider": "nmi", "payment_token": w.nmi.Tokenize(mastercard), "last_four": mastercard.Last4, "card_type": mastercard.Brand, "expiry_date": "12/35"})
				refused(t, status, body, "replace Alice's card")
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
			require.Equal(t, pendingStatus, unwrap(alice.must(http.MethodGet, "/checkout/"+pending.ID, "", nil))["status"], "Alice's checkout is untouched")

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
	rt     *openrails.Client
	server *httptest.Server
	client *openrails.Client
}

func (w *world) rival() *rival {
	// Its own identity provider: merchant A's credentials mean nothing there.
	return w.peer("rival-"+uuid.NewString()[:8], openrails.CustomerBillingManagement, &verifier{secret: []byte("rival-" + uuid.NewString())}, map[string]openrails.PSPConfig{
		"stripe": {"stripe": {AccountID: "acct_rival", Secrets: map[string]string{"secret_key": "sk_test_rival", "webhook_signing_secret": "whsec_rival"}}},
		"nmi":    {"nmi": {AccountID: "rival-nmi", Secrets: map[string]string{"security_key": "rival-nmi-key", "webhook_signing_secret": "nmi_webhook_rival"}, Settings: map[string]any{"tokenization_key": "rival-tokenization"}}},
	})
}

// sibling is another process of the same merchant on the same database, as
// hosts run several replicas behind one load balancer.
func (w *world) sibling() *rival {
	return w.siblingWith(openrails.CustomerBillingManagement)
}

// siblingWith is a sibling publishing the given customer route scope.
func (w *world) siblingWith(scope openrails.CustomerHTTPScope) *rival {
	return w.peer(w.slug, scope, w.auth, w.declaredPSPs())
}

func (w *world) declaredPSPs() map[string]openrails.PSPConfig {
	return map[string]openrails.PSPConfig{
		"stripe": {"stripe": {AccountID: stripeAcct, Secrets: map[string]string{"secret_key": "sk_test_e2e", "webhook_signing_secret": whsecStripe}}},
		"nmi":    {"nmi": {AccountID: nmiAcct, Secrets: map[string]string{"security_key": "e2e-nmi-key", "webhook_signing_secret": whsecNMI}, Settings: map[string]any{"tokenization_key": "e2e-tokenization"}}},
		"ccbill": {"ccbill": {AccountID: ccbillAcct, Secrets: map[string]string{"salt": "e2e-ccbill-salt"}}},
	}
}

// peer is another process on this database. Subjects "auto-<uuid>" are the
// customer's automation credentials, not their interactive session.
func (w *world) peer(slug string, scope openrails.CustomerHTTPScope, v *verifier, psps map[string]openrails.PSPConfig, delegated ...func(*http.Request) (*billingauth.DelegatedPrincipal, error)) *rival {
	t := w.t
	identity, err := billingauth.NewIntegration(billingauth.IntegrationOptions{
		Verifier: v,
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
	routes := openrails.CustomerRoutesConfig{Merchant: slug, Scope: scope}
	deps := hooks(identity)
	if len(delegated) > 0 {
		routes.Delegated = true
		deps.AuthenticateCustomer = func(r *http.Request, _ string) (*billingauth.DelegatedPrincipal, error) { return delegated[0](r) }
	}
	deps.Postgres, deps.StripeTransport, deps.NMITransport, deps.Clock = w.pool, w.stripe, w.nmi, w.clock
	rt, err := openrails.New(t.Context(), openrails.Config{
		Schema: w.schema, River: openrails.RiverHostOwned,
		TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull, AllowCatalogUpdates: true,
		DB: &openrails.DBConfig{URL: w.dsn}, TrustedProxies: []string{"127.0.0.1/32"}, ReturnOrigins: []string{"https://e2e.test"},
		HTTP:     &openrails.HTTPConfig{MerchantAdmin: true, MerchantAPI: true, Catalog: true, CustomerRoutes: []openrails.CustomerRoutesConfig{routes}},
		Merchant: openrails.MerchantDeclaration{Slug: slug, DisplayName: slug, PSPs: psps},
	}, deps)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	if slug != w.slug {
		return w.serve(slug, rt)
	}
	// A replica binds its own River producer and workers. Another merchant
	// stays headless: one fleet per merchant runtime in this harness.
	jobs, err := riverkit.New(t.Context(), w.pool, &river.Config{
		Schema: w.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}},
		FetchCooldown: 5 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond,
	}, rt.RiverJobs())
	require.NoError(t, err)
	require.NoError(t, jobs.Start(context.WithoutCancel(t.Context())))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = jobs.StopAndCancel(ctx)
	})
	return w.serve(slug, rt)
}

func (w *world) serve(slug string, rt *openrails.Client) *rival {
	t := w.t
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, rt, mountPrefix))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := rt
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
	require.ErrorIs(t, err, billing.ErrNotFound)
	_, err = r.client.GetPayment(ctx, payment.ID)
	require.ErrorIs(t, err, billing.ErrNotFound)
	_, err = r.client.RefundPayment(ctx, payment.ID, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "rival-refund"})
	require.Error(t, err)
	require.Error(t, r.client.CancelSubscription(ctx, e.sub, billing.CancelSubscriptionRequest{}))
	price, err := w.client[embedded].Prices.Retrieve(ctx, e.price)
	require.NoError(t, err)
	_, err = r.client.Prices.Retrieve(ctx, price.ID)
	require.ErrorIs(t, err, billing.ErrNotFound)
	_, err = r.client.Products.Retrieve(ctx, price.ProductID)
	require.ErrorIs(t, err, billing.ErrNotFound)
	list, err := r.client.ListPayments(ctx, billing.PaymentFilter{CustomerID: e.c.id})
	require.NoError(t, err)
	require.Empty(t, list.Data)

	// Merchant B cannot sell merchant A's price, or charge merchant A's saved card.
	_, err = r.client.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionRequest{
		OfferKind: billing.OfferRecurring, Customer: billing.CheckoutCustomerIdentity{ID: e.c.id}, Entitlement: e.ent, PriceID: e.price,
		IdempotencyKey: "rival-price-" + uuid.NewString(), PaymentOptions: billing.CheckoutPaymentOptions{Rail: "nmi", PaymentMethodID: e.method},
		SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return",
	})
	require.Error(t, err)
	own := r.client
	product, err := own.Products.Create(ctx, &billing.ProductCreateParams{Key: "rival-" + uuid.NewString()[:8], DisplayName: "Rival", EntitlementsSpec: map[string]*int{"content:rival": nil}})
	require.NoError(t, err)
	rivalPrice, err := own.Prices.Create(ctx, &billing.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	_, err = own.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionRequest{
		OfferKind: billing.OfferPermanent, Customer: billing.CheckoutCustomerIdentity{ID: e.c.id}, Entitlement: "content:rival", PriceID: rivalPrice.ID,
		IdempotencyKey: "rival-card-" + uuid.NewString(), PaymentOptions: billing.CheckoutPaymentOptions{Rail: "nmi", PaymentMethodID: e.method},
		SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return",
	})
	require.Error(t, err, "a foreign merchant's saved card is not chargeable")
	require.NotContains(t, err.Error(), "River", "refused by ownership, not by the headless harness")
	require.Len(t, e.providerLedger(), 1, "nothing charged the member's card")

	// Staff authorized for merchant A cannot select merchant B on either
	// merchant's mount; the same request for A is allowed.
	merchantGet := func(server, token, slug string) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+mountPrefix+"/v1/merchant/findings", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("OpenRails-Merchant", slug)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		res.Body.Close()
		return res.StatusCode
	}
	staff := w.auth.token(t, "staff")
	require.Equal(t, http.StatusOK, merchantGet(w.server.URL, staff, w.slug))
	// A staff session revoked after its token was minted is a credential
	// failure: the live permission check finds it, and the answer is 401,
	// never the 503 of an authorization outage.
	sid := uuid.NewString()
	session := w.auth.sessionToken(t, "staff", sid)
	status, body := w.merchantCall(session, http.MethodGet, "/v1/merchant/findings")
	require.Equal(t, http.StatusOK, status, body)
	w.auth.revoked.Store(sid, struct{}{})
	status, body = w.merchantCall(session, http.MethodGet, "/v1/merchant/findings")
	require.Equal(t, http.StatusUnauthorized, status, body)
	require.Contains(t, body, `"credential_revoked"`)
	require.Equal(t, http.StatusOK, merchantGet(w.server.URL, staff, w.slug), "another session is unaffected")
	for _, server := range []string{w.server.URL, r.server.URL} {
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict}, merchantGet(server, staff, r.slug))
	}
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, merchantGet(w.server.URL, e.c.token, w.slug), "a customer credential is not merchant authority")
	crossed, err := openrails.NewRemote(w.server.URL+mountPrefix, openrails.WithDefaultMerchant(r.slug),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return w.auth.token(t, "staff"), nil }))
	require.NoError(t, err)
	_, err = crossed.GetSubscription(ctx, e.sub)
	require.Error(t, err)
	_, err = crossed.RefundPayment(ctx, payment.ID, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "crossed-refund"})
	require.Error(t, err)

	// A customer credential is never merchant authority.
	customerRemote, err := openrails.NewRemote(w.server.URL+mountPrefix, openrails.WithDefaultMerchant(w.slug),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return e.c.token, nil }))
	require.NoError(t, err)
	_, err = customerRemote.RefundPayment(ctx, payment.ID, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "customer-refund"})
	require.Error(t, err)
	_, err = customerRemote.ListPayments(ctx, billing.PaymentFilter{})
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
