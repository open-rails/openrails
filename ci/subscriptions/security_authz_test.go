//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
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
			aliceSub := alice.subscribe(embedded, rail, price.ID.String(), "content:members", aliceCard)
			malloryCard := mallory.saveCard(rail, mastercard)
			mallorySub := mallory.subscribe(embedded, rail, price.ID.String(), "content:members", malloryCard)
			charges := len(w.railLedger(rail))

			for _, tc := range []struct {
				method, path string
				body         any
			}{
				{http.MethodGet, "/subscriptions/" + aliceSub.String(), nil},
				{http.MethodPost, "/subscriptions/" + aliceSub.String() + "/cancel", map[string]any{"reason": "no longer needed"}},
				{http.MethodPost, "/subscriptions/" + aliceSub.String() + "/resume", map[string]any{}},
				{http.MethodPost, "/subscriptions/" + aliceSub.String() + "/retry-now", map[string]any{}},
				{http.MethodPut, "/subscriptions/" + aliceSub.String() + "/payment-method", map[string]any{"payment_method_id": malloryCard}},
				{http.MethodPut, "/subscriptions/" + mallorySub.String() + "/payment-method", map[string]any{"payment_method_id": aliceCard}},

				{http.MethodDelete, "/payment-methods/" + aliceCard, nil},
			} {
				status, body := mallory.call(tc.method, tc.path, "idor-"+uuid.NewString(), tc.body)
				refused(t, status, body, fmt.Sprintf("%s %s %v", tc.method, tc.path, tc.body))
			}
			// A foreign card is as ineligible as a missing one.
			status, body := mallory.call(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": aliceCard})
			require.Contains(t, []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound}, status, "%v", body)
			_, missing := mallory.call(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": "pm_" + uuid.NewString()})
			require.Equal(t, fmt.Sprint(missing["error"].(map[string]any)["message"]), fmt.Sprint(body["error"].(map[string]any)["message"]))
			if rail == "nmi" {
				status, body := mallory.call(http.MethodPut, "/payment-methods/"+aliceCard, "", map[string]any{"payment_token": w.nmi.Tokenize(mastercard)})
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
			require.Equal(t, billing.SubscriptionActive, sub.Status)
			require.False(t, sub.CancelScheduled)
			require.Nil(t, sub.CanceledAt)
			require.True(t, alice.entitled("content:members"))
			require.Len(t, w.railLedger(rail), charges, "no request charged anyone")

			// Each renewal is paid by its own member's card.
			w.advanceHealthyTo(w.subscription(embedded, mallorySub).CurrentPeriodEndsAt.Add(time.Nanosecond))
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
	auth   *verifier
}

func (w *world) rival() *rival {
	// Its own identity provider: merchant A's credentials mean nothing there.
	return w.peer("rival-"+uuid.NewString()[:8], &verifier{secret: []byte("rival-" + uuid.NewString())}, map[string]openrails.PSPConfig{
		"stripe": {Rail: "stripe", AccountID: "acct_rival", Secrets: map[string]string{"secret_key": "sk_test_rival", "webhook_signing_secret": "whsec_rival"}},
		"nmi":    {Rail: "nmi", AccountID: "rival-nmi", Secrets: map[string]string{"security_key": "rival-nmi-key", "webhook_signing_secret": "nmi_webhook_rival"}, Settings: map[string]any{"tokenization_key": "rival-tokenization"}},
	})
}

// sibling is another process of the same merchant on the same database, as
// hosts run several replicas behind one load balancer.
func (w *world) sibling() *rival {
	return w.peer(w.slug, w.auth, w.declaredPSPs())
}

func (w *world) declaredPSPs() map[string]openrails.PSPConfig {
	return map[string]openrails.PSPConfig{
		"stripe": {Rail: "stripe", AccountID: stripeAcct, Secrets: map[string]string{"secret_key": "sk_test_e2e", "webhook_signing_secret": whsecStripe}, Settings: map[string]any{"publishable_key": "pk_test_e2e"}},
		"nmi":    {Rail: "nmi", AccountID: nmiAcct, Secrets: map[string]string{"security_key": "e2e-nmi-key", "webhook_signing_secret": whsecNMI}, Settings: map[string]any{"tokenization_key": "e2e-tokenization"}},
		"ccbill": {Rail: "ccbill", AccountID: ccbillAcct, Secrets: map[string]string{"salt": "e2e-ccbill-salt"}},
	}
}

// peer is another process on this database, guarded by v.
func (w *world) peer(slug string, v *verifier, psps map[string]openrails.PSPConfig) *rival {
	t := w.t
	deps := openrails.Deps{Postgres: w.pool, StripeTransport: w.stripe, NMITransport: w.nmi, Clock: w.clock}
	rt, err := openrails.New(t.Context(), openrails.Config{
		Database: openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema},
		TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull,
		DB: &openrails.DBConfig{URL: w.dsn}, TrustedProxies: []string{"127.0.0.1/32"}, ReturnOrigins: []string{"https://e2e.test"},
		Merchant: openrails.MerchantDeclaration{Slug: slug, DisplayName: slug, PSPs: psps},
	}, deps)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	routes := openrails.Routes{Auth: v, Prefix: mountPrefix, Permissions: permissions}
	if slug != w.slug {
		return w.serve(slug, rt, routes)
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
	return w.serve(slug, rt, routes)
}

func (w *world) serve(slug string, rt *openrails.Client, routes openrails.Routes) *rival {
	t := w.t
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, rt, routes))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := rt
	auth, _ := routes.Auth.(*verifier)
	return &rival{slug: slug, rt: rt, server: server, client: client, auth: auth}
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
	_, err = r.client.CancelSubscription(ctx, e.sub, billing.CancelSubscriptionParams{})
	require.Error(t, err)
	price, err := w.client[embedded].GetPrice(ctx, typedPriceID(t, e.price), billing.GetPriceParams{})
	require.NoError(t, err)
	_, err = r.client.GetPrice(ctx, price.ID, billing.GetPriceParams{})
	require.ErrorIs(t, err, billing.ErrNotFound)
	_, err = r.client.GetProduct(ctx, price.ProductID)
	require.ErrorIs(t, err, billing.ErrNotFound)
	list, err := r.client.ListPayments(ctx, billing.PaymentListParams{CustomerID: e.c.cid()})
	require.NoError(t, err)
	require.Empty(t, list.Items)

	// Merchant B cannot sell merchant A's price, or charge merchant A's saved card.
	_, err = r.client.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionParams{Customer: e.c.identity(), PriceID: pid(e.price), SuccessURL: "https://e2e.test/return"})
	require.Error(t, err)
	own := r.client
	product, err := own.CreateProduct(ctx, billing.CreateProductParams{Key: "rival-" + uuid.NewString()[:8], DisplayName: "Rival", Entitlements: []string{"content:rival"}})
	require.NoError(t, err)
	rivalPrice, err := own.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	link, err := own.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionParams{Customer: e.c.identity(), PriceID: rivalPrice.ID, SuccessURL: "https://e2e.test/return"})
	require.NoError(t, err)
	// The same subject signed in at merchant B names merchant A's card.
	there := &customer{w: w, id: e.c.id, token: r.auth.token(t, e.c.id)}
	_, err = hostedSession{w: w, id: link.ID}.buyAt(r.server.URL, there, order{rail: "nmi", method: e.method})
	require.Error(t, err, "a foreign merchant's saved card is not chargeable")
	require.NotContains(t, err.Error(), "River", "refused by ownership, not by the headless harness")
	require.Len(t, e.providerLedger(), 1, "nothing charged the member's card")

	// Staff authorized for merchant A cannot select merchant B on either
	// merchant's mount; the same request for A is allowed.
	merchantGet := func(server, token, slug string) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+mountPrefix+"/v1/admin/findings", nil)
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
	status, body := w.merchantCall(session, http.MethodGet, "/v1/admin/findings")
	require.Equal(t, http.StatusOK, status, body)
	w.auth.revoked.Store(sid, struct{}{})
	status, body = w.merchantCall(session, http.MethodGet, "/v1/admin/findings")
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
	_, err = customerRemote.ListPayments(ctx, billing.PaymentListParams{})
	require.Error(t, err)

	w.settle()
	got, err := w.client[embedded].GetPayment(ctx, payment.ID)
	require.NoError(t, err)
	require.Zero(t, got.AmountRefunded)
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)
	for _, entry := range e.providerLedger() {
		require.Zero(t, entry.Refunded)
	}
}
