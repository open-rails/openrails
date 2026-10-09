//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/openrailstest"
)

// Staff may hand a customer a checkout session, as the merchant's server
// may; only the customer pays it, and a saved card only with the customer's
// own proof. The machine checkout-attempt routes are gone.
func TestStaffCheckoutSessionIsTheCustomersToPay(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.start()
	price := w.membership("content:members", 9_990_000)
	a, b := w.newCustomer(), w.newCustomer()
	aCard, bCard := a.saveCard("nmi", visa), b.saveCard("nmi", mastercard)
	charges := len(w.railLedger("nmi"))

	staff := w.auth.token(t, "support")
	attempt := billing.CheckoutAttemptID(uuid.New()).String()
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/v1/admin/checkout-attempts"},
		{http.MethodGet, "/v1/admin/checkout-attempts/" + attempt},
		{http.MethodPost, "/v1/admin/checkout-attempts/" + attempt + "/confirm"},
	} {
		status, out := w.merchantCall(staff, route.method, route.path)
		require.Equal(t, http.StatusNotFound, status, "%s %s is not mounted: %s", route.method, route.path, out)
	}

	// The checkout-sessions route is a staff write: a reader is refused.
	mint := map[string]any{"customer": map[string]any{"id": a.id}, "price_id": price.ID}
	status, out := w.merchantJSON(w.auth.token(t, "reader"), http.MethodPost, "/v1/admin/checkout-sessions", mint)
	require.Equal(t, http.StatusForbidden, status, "%v", out)
	status, out = w.merchantJSON(staff, http.MethodPost, "/v1/admin/checkout-sessions", mint)
	require.Equal(t, http.StatusCreated, status, "%v", out)
	staffSession := hostedSession{w: w, id: out["id"].(string)}
	status, out = staffSession.pay(map[string]any{"option_id": staffSession.option("nmi"), "payment_method_id": aCard})
	require.Equal(t, http.StatusForbidden, status, "the staff member's link alone charges no saved card: %v", out)
	require.Equal(t, "customer_proof_required", hostedErrorCode(out))
	w.settle()
	require.Len(t, w.railLedger("nmi"), charges, "the refused link charged nothing")

	// The host's backend mints one too; only A, signed in, pays it with A's card.
	session := w.handOver(a, price.ID)
	option := session.option("nmi")
	for name, try := range map[string]func() (int, map[string]any){
		"B's credential, A's card": func() (int, map[string]any) {
			return session.payAs(b, map[string]any{"option_id": option, "payment_method_id": aCard})
		},
		"B's credential, B's card": func() (int, map[string]any) {
			return session.payAs(b, map[string]any{"option_id": option, "payment_method_id": bCard})
		},
		"no credential, A's card": func() (int, map[string]any) {
			return session.pay(map[string]any{"option_id": option, "payment_method_id": aCard})
		},
		"A's API key, A's card": func() (int, map[string]any) {
			return session.w.page(http.MethodPost, "/v1/checkout-sessions/"+session.id+"/pay", w.auth.apiKeyToken(t, a.id), map[string]any{"option_id": option, "payment_method_id": aCard})
		},
	} {
		status, out := try()
		require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity}, status, "%s: %v", name, out)
	}
	w.settle()
	require.Len(t, w.railLedger("nmi"), charges, "no refused proof charged anything")
	require.False(t, a.entitled("content:members"))

	a.payWithSaved(session, "nmi", aCard)
	require.Len(t, w.railLedger("nmi"), charges+1)
	require.True(t, a.entitled("content:members"))
	require.False(t, b.entitled("content:members"))
}

// The customer is the subject, whatever credential they signed in with: a
// session and a device key reach the same billing.
func TestCustomerIsTheSubjectWhateverTheCredential(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	card := c.saveCard("nmi", visa)
	device := w.auth.issue(t, grant{subject: c.id, credential: string(openrails.CredentialDeviceKey), sid: "dk_" + uuid.NewString()[:8]})
	for name, token := range map[string]string{"session": c.token, "device key": device} {
		status, out := w.callAt(w.server.URL, token, http.MethodGet, "/payment-methods", "", nil)
		require.Equal(t, http.StatusOK, status, "%s: %v", name, out)
		data, _ := out["data"].([]any)
		require.Len(t, data, 1, name)
		require.Equal(t, card, data[0].(map[string]any)["id"], name)
	}
}

// Only the customer's own credential spends its balance: an invoker acting for
// it is refused admission and cannot read the account.
func TestDelegatedInvokersNeverSpendTheBalance(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx, host := t.Context(), w.client[remote]
	const balance, cozy = int64(600_000), "https://cozy.example"
	app := w.newCustomer()
	_, err := createCreditGrant(ctx, host, app.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: balance, Source: "test", SourceID: "seed"})
	require.NoError(t, err)
	deadline := w.clock.Now().Add(time.Hour)
	verdicts, err := host.Admit(ctx, []billing.AdmitParams{{RequestID: uuid.NewString(), CustomerID: app.cid(), Invoker: cozy + "|u_1", InvokerType: billing.InvokerTypeDelegated,
		Currency: "USD", EstimatedAmount: 100_000, ExpiresAt: &deadline}})
	require.NoError(t, err)
	require.False(t, verdicts[0].Allowed())
	require.NotNil(t, verdicts[0].Admission)
	require.Equal(t, "delegated_spend_not_allowed", *verdicts[0].Admission.DenyCode)
	got, err := host.GetBalance(ctx, app.cid(), "USD")
	require.NoError(t, err)
	require.Zero(t, got.HeldAmount)

	token := w.auth.issue(t, grant{subject: app.id, kind: string(openrails.SubjectApplication), credential: string(openrails.CredentialSignedToken), invokerIssuer: cozy, invoker: "u_1"})
	status, out := w.callAt(w.server.URL, token, http.MethodGet, "", "", nil)
	require.Equal(t, http.StatusForbidden, status, "an invoker does not manage the account: %v", out)
	code, _ := errorOf(out)
	require.Equal(t, billing.CodeInvokerScopedPrincipal, code)
}

// A provider write records who made it: the subject whose authority it used,
// the invoker that acted (the key its rate ceiling counts) and the credential.
func TestIntentsRecordSubjectInvokerAndCredential(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	latest := func() [3]string {
		t.Helper()
		var subject, invoker, credential *string
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT subject, actor, credential FROM billing.provider_intents
			WHERE origin IN ('user', 'admin') ORDER BY created_at DESC, id DESC LIMIT 1`)).Scan(&subject, &invoker, &credential))
		require.NotNil(t, subject)
		require.NotNil(t, invoker)
		require.NotNil(t, credential)
		return [3]string{*subject, *invoker, *credential}
	}
	refund := func(token string) {
		t.Helper()
		e := enroll(t, w, "nmi", embedded)
		payment := completed(w.payments(embedded, e.c.id))[0]
		client, err := openrails.NewRemote(w.server.URL+mountPrefix, openrails.WithDefaultMerchant(w.slug),
			openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }))
		require.NoError(t, err)
		_, err = client.RefundPayment(t.Context(), payment.ID, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "audit-" + uuid.NewString()})
		require.NoError(t, err)
	}

	refund(w.auth.sessionToken(t, "staff", "s_audit"))
	require.Equal(t, [3]string{"staff", "staff", "session:s_audit"}, latest())

	refund(w.auth.issue(t, grant{subject: hostApp, kind: string(openrails.SubjectApplication), credential: string(openrails.CredentialAPIKey), sid: "key_ops", invokerIssuer: "https://ops.example", invoker: "ops_7"}))
	require.Equal(t, [3]string{hostApp, "https://ops.example|ops_7", "api_key:key_ops"}, latest())

	c := w.newCustomer()
	card := c.saveCard("nmi", visa)
	device := w.auth.issue(t, grant{subject: c.id, credential: string(openrails.CredentialDeviceKey), sid: "dk_self"})
	status, out := w.callAt(w.server.URL, device, http.MethodDelete, "/payment-methods/"+card, "", nil)
	require.Less(t, status, 300, "%v", out)
	require.Equal(t, [3]string{c.id, c.id, "device_key:dk_self"}, latest())
}

// IDOR: a signed-in customer naming another customer's object on any
// customer route, or on a checkout session, is refused and changes nothing.
// Every such route needs a fixture here: a new one fails until it has one.
func TestCustomerRoutesRefuseAnotherCustomersObjects(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.start()
	ctx := t.Context()
	group := "g" + uuid.NewString()[:8]
	from := w.tierPrice(group, 1, 1000, monthHours, false)
	to := w.tierPrice(group, 2, 2000, monthHours, false)

	// A's objects: a membership with an upgrade awaiting authentication, a
	// saved card and a card setup, an open invoice, a notification and a
	// checkout session.
	a, sub := w.engineMember("stripe", embedded, from)
	w.stripe.SetDecline(visa.Last4, "auth")
	pending, err := a.change(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(to.ID), IdempotencyKey: "idor-" + uuid.NewString()})
	require.NoError(t, err)
	require.Equal(t, "requires_action", pending.Status)
	aCard := a.saveCard("nmi", visa)
	setup := a.must(http.MethodPost, "/payment-method-setups", "setup-"+uuid.NewString(), map[string]any{"psp_id": w.psp["stripe"], "consent": true})["id"].(string)
	_, err = w.client[remote].UpdateCustomer(ctx, a.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 100_000_000}}})
	require.NoError(t, err)
	_, err = recordUsage(ctx, w.client[remote], billing.RecordUsageParams{CustomerID: a.cid(), Invoker: a.id, Currency: "USD", EventType: "idor", Amount: 50_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	w.advance(time.Minute)
	job, err := w.jobs.Insert(ctx, invoicePass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(job.Job.ID)
	invoices, err := w.client[embedded].ListInvoices(ctx, billing.InvoiceListParams{CustomerID: a.cid()})
	require.NoError(t, err)
	require.NotEmpty(t, invoices.Items)
	invoice := invoices.Items[0].ID.String()
	_, err = w.pool.Exec(ctx, w.q(`INSERT INTO billing.notifications (merchant_id, customer_id, event_type, data)
		SELECT id, $2, 'test.idor', '{}' FROM billing.merchants WHERE slug = $1`), w.slug, a.cid().UUID())
	require.NoError(t, err)
	notes, _ := a.must(http.MethodGet, "/notifications?limit=10", "", nil)["data"].([]any)
	require.NotEmpty(t, notes)
	notification := notes[0].(map[string]any)["id"].(string)
	session := w.handOver(a, from.ID)
	pass := w.lifetime("idor:order", 3_000_000)
	aOrder := a.must(http.MethodPost, "/orders", "order-"+uuid.NewString(), map[string]any{"lines": []any{map[string]any{"price_id": pass.ID.String()}}})["id"].(string)

	b := w.newCustomer()
	bCard := b.saveCard("nmi", mastercard)
	before := w.railLedger("stripe")
	nmiBefore := w.railLedger("nmi")

	op := pending.OperationID.String()
	probes := map[string]struct {
		path string
		body any
	}{
		"POST /v1/me/subscriptions/{id}/cancel":                      {"/subscriptions/" + sub.String() + "/cancel", map[string]any{"reason": "not mine"}},
		"POST /v1/me/subscriptions/{id}/resume":                      {"/subscriptions/" + sub.String() + "/resume", map[string]any{}},
		"PUT /v1/me/subscriptions/{id}/payment-method":               {"/subscriptions/" + sub.String() + "/payment-method", map[string]any{"payment_method_id": bCard}},
		"GET /v1/me/subscriptions/{id}":                              {"/subscriptions/" + sub.String(), nil},
		"POST /v1/me/subscriptions/{id}/retry-now":                   {"/subscriptions/" + sub.String() + "/retry-now", map[string]any{}},
		"POST /v1/me/subscriptions/{id}/change":                      {"/subscriptions/" + sub.String() + "/change", map[string]any{"price_id": to.ID}},
		"POST /v1/me/subscriptions/{id}/change/preview":              {"/subscriptions/" + sub.String() + "/change/preview", map[string]any{"price_id": to.ID}},
		"GET /v1/me/payment-operations/{id}/authentication":          {"/payment-operations/" + op + "/authentication", nil},
		"POST /v1/me/payment-operations/{id}/authentication/confirm": {"/payment-operations/" + op + "/authentication/confirm", map[string]any{}},
		"GET /v1/me/invoices/{id}":                                   {"/invoices/" + invoice, nil},
		"POST /v1/me/invoices/{id}/pay-now":                          {"/invoices/" + invoice + "/pay-now", map[string]any{"payment_method_id": bCard}},
		"PUT /v1/me/payment-methods/{id}":                            {"/payment-methods/" + aCard, map[string]any{"payment_token": w.nmi.Tokenize(mastercard), "billing_details": map[string]any{"name": "Not Mine"}}},
		"PATCH /v1/me/payment-methods/{id}":                          {"/payment-methods/" + aCard, map[string]any{"exp_month": 12, "exp_year": 2039, "reusable": false}},
		"POST /v1/me/payment-methods/{id}/verify":                    {"/payment-methods/" + aCard + "/verify", nil},
		"DELETE /v1/me/payment-methods/{id}":                         {"/payment-methods/" + aCard, nil},
		"PUT /v1/me/default-payment-methods/{currency}":              {"/default-payment-methods/USD", map[string]any{"payment_method_id": aCard}},
		"GET /v1/me/payment-method-setups/{id}":                      {"/payment-method-setups/" + setup, nil},
		"POST /v1/me/payment-method-setups/{id}/confirm":             {"/payment-method-setups/" + setup + "/confirm", map[string]any{}},
		"GET /v1/me/checkout-sessions/{id}":                          {"/checkout-sessions/" + session.id, nil},
		"POST /v1/me/checkout-sessions/{id}/pay":                     {"/checkout-sessions/" + session.id + "/pay", map[string]any{"option_id": session.option("nmi"), "payment_method_id": bCard}},
		"GET /v1/me/orders/{id}":                                     {"/orders/" + aOrder, nil},
		"POST /v1/me/orders/{id}/pay":                                {"/orders/" + aOrder + "/pay", map[string]any{"payment": map[string]any{"payment_method_id": bCard}, "expected_total": "3000000"}},
		"POST /v1/me/orders/{id}/confirm":                            {"/orders/" + aOrder + "/confirm", nil},
		"POST /v1/me/orders/{id}/cancel":                             {"/orders/" + aOrder + "/cancel", nil},
	}
	probed := 0
	for _, route := range routes.Catalog() {
		if route.Group != routes.Customer || !strings.Contains(route.Path, "{") {
			continue
		}
		probe, ok := probes[route.Key()]
		require.True(t, ok, "%s names an object and has no IDOR fixture", route.Key())
		status, out := w.callAt(w.server.URL, b.token, route.Method, probe.path, "idor-"+uuid.NewString(), probe.body)
		require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, status, "%s with A's id: %v", route.Key(), out)
		probed++
	}
	require.Len(t, probes, probed, "every fixture names a mounted route")

	// Routes naming objects in their body: A's ids are answered null and
	// left untouched. Every such route needs a fixture here.
	bodyProbes := map[string]struct {
		path string
		body any
		ids  string
		id   string
	}{
		"POST /v1/me/notifications/read": {"/notifications/read", map[string]any{"notification_ids": []string{notification}}, "notifications", notification},
	}
	probed = 0
	for _, route := range routes.Catalog() {
		if route.Group != routes.Customer || !namesObjectsInBody(route.Request) {
			continue
		}
		probe, ok := bodyProbes[route.Key()]
		require.True(t, ok, "%s names objects in its body and has no IDOR fixture", route.Key())
		status, out := w.callAt(w.server.URL, b.token, route.Method, probe.path, "idor-"+uuid.NewString(), probe.body)
		require.Equal(t, http.StatusOK, status, "%s with A's id: %v", route.Key(), out)
		answered := out[probe.ids].(map[string]any)
		require.Contains(t, answered, probe.id, "%s answers every requested id", route.Key())
		require.Nil(t, answered[probe.id], "%s with A's id: %v", route.Key(), out)
		probed++
	}
	require.Len(t, bodyProbes, probed, "every body fixture names a mounted route")

	read := session.read()
	require.Empty(t, read["saved_methods"], "a guest sees no saved cards")
	status, out := w.page(http.MethodGet, "/v1/checkout-sessions/"+session.id, b.token, nil)
	require.Equal(t, http.StatusNotFound, status, "to B, A's session does not exist: %v", out)
	status, out = session.payAs(b, map[string]any{"option_id": session.option("nmi"), "payment_method_id": bCard})
	require.Equal(t, http.StatusNotFound, status, "B paying A's session: %v", out)

	// Nothing of A's changed.
	w.settle()
	require.Equal(t, before, w.railLedger("stripe"))
	require.Equal(t, nmiBefore, w.railLedger("nmi"))
	got := w.subscription(embedded, sub)
	require.Equal(t, billing.SubscriptionActive, got.Status)
	require.Equal(t, from.ID, got.PriceID)
	require.Nil(t, got.CanceledAt)
	methods, err := w.client[embedded].ListPaymentMethods(ctx, a.cid(), billing.PaymentMethodListParams{})
	require.NoError(t, err)
	require.Len(t, methods.Items, 2, "A's cards are A's")
	again, err := w.client[embedded].GetInvoice(ctx, invoices.Items[0].ID)
	require.NoError(t, err)
	require.Equal(t, invoices.Items[0].AmountDue, again.AmountDue)
	status, everything := w.callAt(w.server.URL, b.token, http.MethodPost, "/notifications/read", "", map[string]any{"all": true})
	require.Equal(t, http.StatusOK, status, "%v", everything)
	require.Empty(t, everything["notifications"], "marking all read names none")
	unread := a.must(http.MethodGet, "/notifications?limit=10", "", nil)["data"].([]any)[0].(map[string]any)
	require.Equal(t, false, unread["seen"], "A's notification is still unread: B marked only B's own")
	marked := a.must(http.MethodPost, "/notifications/read", "", map[string]any{"notification_ids": []string{notification}})["notifications"].(map[string]any)
	require.Equal(t, true, marked[notification].(map[string]any)["seen"], "A marks it read")
	_, err = w.pool.Exec(ctx, w.q(`INSERT INTO billing.notifications (merchant_id, customer_id, event_type, data)
		SELECT id, $2, 'test.idor', '{}' FROM billing.merchants WHERE slug = $1`), w.slug, a.cid().UUID())
	require.NoError(t, err)
	a.must(http.MethodPost, "/notifications/read", "", map[string]any{"all": true})
	for _, note := range a.must(http.MethodGet, "/notifications?limit=10", "", nil)["data"].([]any) {
		require.Equal(t, true, note.(map[string]any)["seen"], "A marks all read")
	}
	status, refused := w.callAt(w.server.URL, a.token, http.MethodPost, "/notifications/read", "", map[string]any{"all": true, "notification_ids": []string{notification}})
	require.Equal(t, http.StatusBadRequest, status, "all names no ids: %v", refused)
	require.NotEmpty(t, unwrap(a.must(http.MethodGet, "/payment-operations/"+op+"/authentication", "", nil))["client_secret"], "A's upgrade still waits for A")
	require.Equal(t, "open", a.must(http.MethodGet, "/orders/"+aOrder, "", nil)["status"], "A's order is still A's to pay")
}

// The harness's own Auth passes the conformance kit a host runs in its CI.
func TestHarnessAuthConforms(t *testing.T) {
	t.Parallel()
	v := &verifier{secret: []byte("conformance-" + uuid.NewString())}
	stranger := &verifier{secret: []byte("stranger-" + uuid.NewString())}
	customer := uuid.NewString()
	signedOut := v.sessionToken(t, customer, "s_out")
	v.revoked.Store("s_out", struct{}{})
	req := func(token string) func() *http.Request {
		return func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, mountPrefix+"/v1/admin/payments/pay_x/refunds", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			return r
		}
	}
	openrailstest.CheckAuth(t, v, openrailstest.AuthCases{
		Permissions:  permissions,
		Programmatic: true,
		Customer:     req(v.token(t, customer)),
		Staff:        req(v.token(t, "staff")),
		Holders:      map[string]func() *http.Request{staffReads.String(): req(v.token(t, "reader"))},
		Refused: map[string]func() *http.Request{
			"forged": req(v.token(t, customer) + "x"), "another issuer's": req(stranger.token(t, customer)), "signed out": req(signedOut),
		},
		StaleStaff: req(v.staleToken(t, "staff")),
		Machine:    req(v.hostToken(t)),
	})
}

// namesObjectsInBody reports a request body carrying a list of object ids.
func namesObjectsInBody(request any) bool {
	if request == nil {
		return false
	}
	t := reflect.TypeOf(request)
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if strings.HasSuffix(name, "_ids") {
			return true
		}
	}
	return false
}
