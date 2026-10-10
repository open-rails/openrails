//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/helpers/smtp/smtptest"
	"github.com/open-rails/helpers/userinfo"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/openrailstest"
)

// hostedCheckout is Stripe hosted Checkout in front of the world's Stripe
// fake: it opens Checkout Sessions and reports them paid.
type hostedCheckout struct {
	http.RoundTripper
	mu       sync.Mutex
	sessions []url.Values
}

func (h *hostedCheckout) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/checkout/sessions" {
		return h.RoundTripper.RoundTrip(r)
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.sessions = append(h.sessions, form)
	id := fmt.Sprintf("cs_test_receipt_%d", len(h.sessions))
	h.mu.Unlock()
	body := fmt.Sprintf(`{"id":%q,"object":"checkout.session","url":"https://checkout.stripe.test/%s"}`, id, id)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// completed is Stripe reporting the n-th session paid.
func (h *hostedCheckout) completed(n int) obj {
	h.mu.Lock()
	form := h.sessions[n-1]
	h.mu.Unlock()
	amount, _ := strconv.ParseInt(form.Get("line_items[0][price_data][unit_amount]"), 10, 64)
	metadata := map[string]string{}
	for _, key := range []string{"user_id", "internal_price_id", "checkout_attempt_id"} {
		metadata[key] = form.Get("metadata[" + key + "]")
	}
	return stripeEvent("checkout.session.completed", obj{"object": "checkout.session", "id": fmt.Sprintf("cs_test_receipt_%d", n),
		"mode": "payment", "status": "complete", "payment_status": "paid", "payment_intent": fmt.Sprintf("pi_receipt_%d", n),
		"amount_total": amount, "currency": form.Get("line_items[0][price_data][currency]"), "customer": form.Get("customer"), "metadata": metadata})
}

// receiptWorld is a world whose customers' receipts go over SMTP to the
// addresses the host's directory holds.
func receiptWorld(t *testing.T, hosted *hostedCheckout) (*world, *smtptest.Server, *openrailstest.UserInfo) {
	t.Helper()
	srv := smtptest.Start(t, smtptest.Options{Username: "apikey", Password: "SG.e2e-key"})
	directory := &openrailstest.UserInfo{}
	w := prepareWorld(t, 12, func(c *openrails.Config) {
		c.SMTP = &openrails.SMTPConfig{Host: srv.Host, Port: srv.Port, Username: "apikey", Password: "SG.e2e-key",
			From: openrails.EmailAddress{Name: "Merchant Billing", Address: "noreply@deploy.test"}}
	})
	if hosted != nil {
		// Without a publishable key, Stripe sells through hosted Checkout.
		w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps["stripe"].Settings, "publishable_key") }
	}
	w.deps = func(d *openrails.Deps) {
		d.UserInfo = directory
		if hosted != nil {
			hosted.RoundTripper = d.StripeTransport
			d.StripeTransport = hosted
		}
	}
	w.start()
	require.NoError(t, w.applySettings(t.Context(), billing.MerchantSettings{Profile: &billing.MerchantProfile{DisplayName: "Host Shop"}}))
	return w, srv, directory
}

type notificationEmail struct{}

func (notificationEmail) Kind() string { return "openrails.notification_email" }

// mailSettled waits for every queued notification email, then sweeps what
// is still undelivered.
func (w *world) mailSettled() {
	w.t.Helper()
	require.Eventually(w.t, func() bool {
		page, err := w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds(notificationEmail{}.Kind()).
			States(rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled, rivertype.JobStatePending).First(10))
		return err == nil && len(page.Jobs) == 0
	}, 20*time.Second, 25*time.Millisecond, "notification emails sent")
	res, err := w.jobs.Insert(w.t.Context(), emailSweep{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
}

// receipts are the customer's one-off purchase receipts in their inbox.
func (c *customer) receipts() []map[string]any {
	c.w.t.Helper()
	var out []map[string]any
	for _, raw := range c.must(http.MethodGet, "/notifications", "", nil)["data"].([]any) {
		if n := raw.(map[string]any); n["event_type"] == "one_off_purchase_completed" {
			out = append(out, n["data"].(map[string]any))
		}
	}
	return out
}

// A customer-chosen credit deposit bought through Stripe hosted Checkout from
// a checkout session: the credit lands and its receipt goes out by SMTP as
// the payment commits, naming what was bought and the amount. Stripe
// delivering the completion again, or under another event, sends nothing.
func TestCreditDepositReceiptArrivesBySMTP(t *testing.T) {
	t.Parallel()
	hosted := &hostedCheckout{}
	w, srv, directory := receiptWorld(t, hosted)
	w.waive("evidenced", "hosted Checkout charges at Stripe, outside the fake's ledger; its completion is the evidence")
	ctx := t.Context()
	client := w.client[embedded]
	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "api-credit-" + uuid.NewString()[:8], DisplayName: "API credit", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", FromPayment: true}})
	require.NoError(t, err)
	price, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "deposit", Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}})
	require.NoError(t, err)
	c := w.newCustomer()
	const to = "buyer@host.test"
	directory.Put(userinfo.User{ID: c.id, Email: to, Username: "buyer"})

	session := c.session(map[string]any{"price_id": price.ID, "amount": "12990000", "success_url": "https://e2e.test/done"})
	status, out := session.payAs(c, map[string]any{"option_id": session.option("stripe")})
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, "requires_action", out["status"], "%v", out)
	require.Empty(t, srv.Messages(), "nothing is paid yet")

	paid := hosted.completed(1)
	require.Equal(t, http.StatusOK, w.deliver("stripe", paid))
	grants, err := client.ListCreditGrants(ctx, c.customerID(), billing.CreditGrantListParams{})
	require.NoError(t, err)
	require.Len(t, grants.Items, 1)
	require.EqualValues(t, 12_990_000, grants.Items[0].Amount, "the chosen amount is the credit")

	m := srv.Wait(t, 1, 20*time.Second)[0]
	require.Equal(t, []string{to}, m.To)
	require.Equal(t, "Your receipt from Host Shop", m.Subject)
	for _, body := range []string{m.Text, m.HTML} {
		require.Contains(t, body, "API credit", "the receipt names what was bought")
		require.Contains(t, body, "12.99 USD", "and the amount, as a person reads it")
		require.NotContains(t, body, "12990000")
	}

	require.Equal(t, http.StatusOK, w.deliver("stripe", paid), "Stripe redelivers the event")
	again := hosted.completed(1)
	again["type"] = "checkout.session.async_payment_succeeded"
	require.Equal(t, http.StatusOK, w.deliver("stripe", again), "another event reports the same payment")
	w.mailSettled()
	require.Len(t, srv.Messages(), 1, "one purchase, one receipt")
	receipts := c.receipts()
	require.Len(t, receipts, 1)
	require.Equal(t, "12990000", receipts[0]["amount"])
	require.Equal(t, "API credit", receipts[0]["product_name"])
	require.True(t, strings.HasPrefix(receipts[0]["payment_id"].(string), billing.PaymentIDPrefix), "%v", receipts[0])
	grants, err = client.ListCreditGrants(ctx, c.customerID(), billing.CreditGrantListParams{})
	require.NoError(t, err)
	require.Len(t, grants.Items, 1)
}

// An NMI one-time price bought through an order: a declined charge sends no
// receipt; the charge that pays the order sends one naming the order, and
// replaying the payment sends no other.
func TestOrderReceiptArrivesBySMTP(t *testing.T) {
	t.Parallel()
	w, srv, directory := receiptWorld(t, nil)
	life := w.lifetime("orders:receipt", 25_000_000)
	c := w.newCustomer()
	const to = "orders@host.test"
	directory.Put(userinfo.User{ID: c.id, Email: to, Username: "orderer"})
	card := c.saveCard("nmi", visa)
	w.nmi.SetDecline(visa.Last4, "202")
	declined := c.order(http.MethodPost, "/orders", "buy-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(25_000_000), "payment": map[string]any{"payment_method_id": card}})
	require.Equal(t, "open", declined.body["status"], "%v", declined.body)
	w.mailSettled()
	require.Empty(t, srv.Messages(), "a declined charge has no receipt")

	good := c.saveCard("nmi", mastercard)
	id, key := declined.body["id"].(string), "pay-"+uuid.NewString()
	body := map[string]any{"payment": map[string]any{"payment_method_id": good}, "expected_total": micros(25_000_000)}
	paid := c.order(http.MethodPost, "/orders/"+id+"/pay", key, body)
	require.Equal(t, "paid", paid.body["status"], "%v", paid.body)
	number := paid.body["number"].(string)

	m := srv.Wait(t, 1, 20*time.Second)[0]
	require.Equal(t, []string{to}, m.To)
	for _, text := range []string{m.Text, m.HTML} {
		require.Contains(t, text, "Lifetime")
		require.Contains(t, text, "25.00 USD")
		require.Contains(t, text, number, "the receipt names the order")
	}
	require.True(t, c.order(http.MethodPost, "/orders/"+id+"/pay", key, body).replayed)
	w.mailSettled()
	require.Len(t, srv.Messages(), 1)
	receipts := c.receipts()
	require.Len(t, receipts, 1)
	require.Equal(t, id, receipts[0]["order_id"])
	require.Equal(t, number, receipts[0]["order_number"])
	require.Equal(t, "nmi", receipts[0]["rail"])
}
