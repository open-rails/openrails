//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Orders: a customer buys priced lines on /v1/me with their own credential
// and a saved card, as billing-ui does on the host's own site.

func orderWorld(t *testing.T) *world {
	t.Helper()
	return newWorld(t)
}

// lifetime is a product bought once and kept.
func (w *world) lifetime(entitlement string, amount int64) *billing.Price {
	w.t.Helper()
	client := w.client[embedded]
	product, err := client.CreateProduct(w.t.Context(), billing.CreateProductParams{Key: "life-" + uuid.NewString()[:8], DisplayName: "Lifetime", Entitlements: []string{entitlement}})
	require.NoError(w.t, err)
	price, err := client.CreatePrice(w.t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: amount, Currency: "USD"})
	require.NoError(w.t, err)
	return price
}

// orderCall is one /v1/me order request and its answer.
type orderCall struct {
	status   int
	header   http.Header
	body     map[string]any
	replayed bool
}

func (c *customer) order(method, path, key string, body any) orderCall {
	c.w.t.Helper()
	var data io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(c.w.t, err)
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(c.w.t.Context(), method, c.w.server.URL+mountPrefix+"/v1/me"+path, data)
	require.NoError(c.w.t, err)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(c.w.t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(c.w.t, err)
	out := orderCall{status: res.StatusCode, header: res.Header, body: map[string]any{}}
	if len(raw) > 0 {
		require.NoError(c.w.t, json.Unmarshal(raw, &out.body), "%s %s: %s", method, path, raw)
	}
	out.replayed = res.Header.Get("Idempotent-Replayed") == "true"
	// A processing order whose charge River's executor is still running is
	// read again once it finishes.
	if id, _ := out.body["id"].(string); method == http.MethodPost && out.body["status"] == "processing" && id != "" {
		if order, err := billing.ParseOrderID(id); err == nil && c.w.awaitExecutor(`i.payload->>'order_id' = $1`, order.UUID().String()) {
			out.body = c.order(http.MethodGet, "/orders/"+id, "", nil).body
		}
	}
	return out
}

// line is one order line; a zero quantity sends none.
func line(price *billing.Price, quantity int) map[string]any {
	out := map[string]any{"price_id": price.ID.String()}
	if quantity > 0 {
		out["quantity"] = quantity
	}
	return out
}

func micros(amount int64) string { return strconv.FormatInt(amount, 10) }

func orderError(call orderCall) string {
	if e, ok := call.body["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		return code
	}
	return ""
}

func errorMeta(call orderCall, key string) string {
	if e, ok := call.body["error"].(map[string]any); ok {
		if meta, ok := e["metadata"].(map[string]any); ok {
			v, _ := meta[key].(string)
			return v
		}
	}
	return ""
}

// orderOf is the order an answer carries: its body, or a failed payment's
// error.order.
func orderOf(call orderCall) map[string]any {
	if e, ok := call.body["error"].(map[string]any); ok {
		if o, ok := e["order"].(map[string]any); ok {
			return o
		}
	}
	return call.body
}

// paymentOf is the payment of the order an answer carries.
func paymentOf(call orderCall) map[string]any {
	p, _ := orderOf(call)["payment"].(map[string]any)
	return p
}

func orderLines(call orderCall) []map[string]any {
	var out []map[string]any
	for _, l := range call.body["lines"].([]any) {
		out = append(out, l.(map[string]any))
	}
	return out
}

// promoteOperations makes scheduled provider operations runnable without
// waiting for them, for a test holding one at the provider.
func (w *world) promoteOperations() {
	page, err := w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation").States(rivertype.JobStateScheduled, rivertype.JobStateRetryable).First(100))
	if err != nil {
		return
	}
	for _, job := range page.Jobs {
		_, _ = w.jobs.JobRetry(w.t.Context(), job.ID)
	}
}

type orderExpiryPass struct{}

func (orderExpiryPass) Kind() string { return "openrails.order_expiry" }

func (w *world) expireOrders() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), orderExpiryPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
}

// The one-call buy: a membership and a lifetime product in one order,
// charged once on a saved NMI card and numbered when paid. The key replays
// the order's current state, refuses another body, and the customer cannot
// buy what they own again.
func TestOrderOneCallBuy(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	ctx := t.Context()
	member := w.membership("orders:seats", 10_000_000)
	life := w.lifetime("orders:lifetime", 25_000_000)
	c := w.newCustomer()
	card := c.saveCard("nmi", visa)
	body := map[string]any{"lines": []any{line(member, 0), line(life, 0)}, "expected_total": micros(35_000_000), "payment": map[string]any{"payment_method_id": card}}

	preview := c.order(http.MethodPost, "/orders/preview", "", map[string]any{"lines": body["lines"]})
	require.Equal(t, http.StatusOK, preview.status, "%v", preview.body)
	require.Equal(t, micros(35_000_000), preview.body["total"])
	seated := c.order(http.MethodPost, "/orders/preview", "", map[string]any{"lines": []any{line(member, 3)}})
	require.Equal(t, "quantity_not_allowed", orderLines(seated)[0]["refusal"].(map[string]any)["code"], "a price without seats takes no quantity")
	require.Equal(t, "quantity_not_allowed", orderError(c.order(http.MethodPost, "/orders", "seated-"+uuid.NewString(), map[string]any{"lines": []any{line(member, 3)}})))
	require.NotEmpty(t, preview.body["payment_options"], "NMI takes these lines")

	require.Equal(t, "idempotency_key_required", orderError(c.order(http.MethodPost, "/orders", "", body)))
	wrong := map[string]any{"lines": body["lines"], "expected_total": micros(1), "payment": body["payment"]}
	require.Equal(t, "order_total_changed", orderError(c.order(http.MethodPost, "/orders", "buy-wrong-"+uuid.NewString(), wrong)))

	key := "buy-" + uuid.NewString()
	bought := c.order(http.MethodPost, "/orders", key, body)
	require.Equal(t, http.StatusCreated, bought.status, "%v", bought.body)
	require.False(t, bought.replayed)
	require.Equal(t, "complete", bought.body["status"], "%v", bought.body)
	require.Equal(t, "succeeded", paymentOf(bought)["status"])
	require.Equal(t, card, paymentOf(bought)["payment_method_id"])
	require.NotNil(t, paymentOf(bought)["payment_id"])
	require.NotEmpty(t, bought.body["number"])
	require.Equal(t, micros(35_000_000), bought.body["total"])
	id := bought.body["id"].(string)
	lines := orderLines(bought)
	require.Len(t, lines, 2)
	require.Nil(t, lines[0]["quantity"], "a membership without seats has no quantity")
	require.EqualValues(t, 1, lines[1]["quantity"])
	require.Equal(t, micros(10_000_000), lines[0]["amount"])
	require.NotNil(t, lines[0]["subscription_id"], "the recurring line made the subscription")
	require.NotNil(t, lines[1]["product_access_id"], "the one-off line made the access")
	require.True(t, c.entitled("orders:seats"))
	require.True(t, c.entitled("orders:lifetime"))
	require.Len(t, w.nmi.saleOrders(), 1, "one charge for the whole order")

	replay := c.order(http.MethodPost, "/orders", key, body)
	require.Equal(t, http.StatusCreated, replay.status, "the stored answer: %v", replay.body)
	require.True(t, replay.replayed)
	require.Equal(t, id, replay.body["id"])
	require.Equal(t, "complete", replay.body["status"])
	require.Len(t, w.nmi.saleOrders(), 1, "a replay never charges")
	other := map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(25_000_000), "payment": body["payment"]}
	reused := c.order(http.MethodPost, "/orders", key, other)
	require.Equal(t, http.StatusUnprocessableEntity, reused.status)
	require.Equal(t, "idempotency_key_reused", orderError(reused))

	again := c.order(http.MethodPost, "/orders", "again-"+uuid.NewString(), other)
	require.Equal(t, http.StatusConflict, again.status, "%v", again.body)
	require.Equal(t, "already_owned", orderError(again))
	require.Contains(t, errorMeta(again, "owned_by"), billing.ProductAccessIDPrefix)
	seats := c.order(http.MethodPost, "/orders", "seats-"+uuid.NewString(), map[string]any{"lines": []any{line(member, 0)}})
	require.Equal(t, "already_owned", orderError(seats))
	require.Contains(t, errorMeta(seats, "owned_by"), billing.SubscriptionIDPrefix)
	require.Equal(t, "change", errorMeta(seats, "hint"))
	refused := c.order(http.MethodPost, "/orders/preview", "", map[string]any{"lines": []any{line(life, 0)}})
	require.Equal(t, "already_owned", orderLines(refused)[0]["refusal"].(map[string]any)["code"])

	read := c.order(http.MethodGet, "/orders/"+id, "", nil)
	require.Equal(t, "complete", read.body["status"])
	list := c.order(http.MethodGet, "/orders", "", nil)
	require.Equal(t, id, list.body["data"].([]any)[0].(map[string]any)["id"])

	events, err := w.client[embedded].ListHostEvents(ctx, billing.HostEventListParams{Type: billing.HostEventOrderCompleted})
	require.NoError(t, err)
	require.Len(t, events.Items, 1)
	require.Equal(t, id, events.Items[0].Order.OrderID.String())
	require.Equal(t, bought.body["number"], *events.Items[0].Order.Number)
	require.Equal(t, billing.OrderComplete, events.Items[0].Order.Status)
	require.Equal(t, billing.OrderPaymentSucceeded, events.Items[0].Order.PaymentStatus)

	// The membership renews from the order's paid period.
	sub, err := billing.ParseSubscriptionID(lines[0]["subscription_id"].(string))
	require.NoError(t, err)
	w.advanceHealthyTo(*w.subscription(embedded, sub).CurrentPeriodEndsAt)
	w.runRenewals()
	var renewal *billing.Payment
	for _, p := range w.payments(embedded, c.id) {
		if p.SubscriptionID != nil && *p.SubscriptionID == sub {
			renewal = &p
		}
	}
	require.NotNil(t, renewal, "the membership renewed")
	require.Equal(t, int64(10_000_000), renewal.Amount)
}

// A decline answers 402 card_error with the order, open with the reason, as
// Stripe answers with its PaymentIntent; the key replays that same answer.
// Paying again with a good card completes it, and a complete order takes no
// more payment. Another customer cannot read, pay or cancel it.
func TestOrderDeclineThenPay(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	life := w.lifetime("orders:declined", 9_000_000)
	c := w.newCustomer()
	card := c.saveCard("nmi", visa)
	w.nmi.SetDecline(visa.Last4, "202")
	buyKey := "buy-" + uuid.NewString()
	buy := map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(9_000_000), "payment": map[string]any{"payment_method_id": card}}
	declined := c.order(http.MethodPost, "/orders", buyKey, buy)
	require.Equal(t, http.StatusPaymentRequired, declined.status, "%v", declined.body)
	require.Equal(t, "card_declined", orderError(declined))
	require.Equal(t, "card_error", declined.body["error"].(map[string]any)["type"])
	require.Equal(t, "open", orderOf(declined)["status"])
	require.Equal(t, "requires_payment_method", paymentOf(declined)["status"])
	require.Equal(t, "insufficient_funds", paymentOf(declined)["last_payment_error"].(map[string]any)["reason"])
	require.False(t, c.entitled("orders:declined"))
	id := orderOf(declined)["id"].(string)
	again := c.order(http.MethodPost, "/orders", buyKey, buy)
	require.Equal(t, http.StatusPaymentRequired, again.status, "the replay is the stored 402: %v", again.body)
	require.True(t, again.replayed)
	require.Equal(t, id, orderOf(again)["id"])
	require.Equal(t, "card_declined", orderError(again))
	require.Len(t, w.nmi.saleOrders(), 1, "a replay never charges")

	b := w.newCustomer()
	bCard := b.saveCard("nmi", mastercard)
	require.Equal(t, http.StatusNotFound, b.order(http.MethodGet, "/orders/"+id, "", nil).status)
	require.Equal(t, http.StatusNotFound, b.order(http.MethodPost, "/orders/"+id+"/pay", "steal-"+uuid.NewString(), map[string]any{"payment": map[string]any{"payment_method_id": bCard}, "expected_total": micros(9_000_000)}).status)
	require.Equal(t, http.StatusNotFound, b.order(http.MethodPost, "/orders/"+id+"/cancel", "", nil).status)

	good := c.saveCard("nmi", mastercard)
	key := "pay-" + uuid.NewString()
	paid := c.order(http.MethodPost, "/orders/"+id+"/pay", key, map[string]any{"payment": map[string]any{"payment_method_id": good}, "expected_total": micros(9_000_000)})
	require.Equal(t, http.StatusOK, paid.status, "%v", paid.body)
	require.Equal(t, "complete", paid.body["status"])
	require.Nil(t, paymentOf(paid)["last_payment_error"])
	require.True(t, c.entitled("orders:declined"))
	replay := c.order(http.MethodPost, "/orders/"+id+"/pay", key, map[string]any{"payment": map[string]any{"payment_method_id": good}, "expected_total": micros(9_000_000)})
	require.True(t, replay.replayed)
	require.Equal(t, "complete", replay.body["status"])
	more := c.order(http.MethodPost, "/orders/"+id+"/pay", "more-"+uuid.NewString(), map[string]any{"payment": map[string]any{"payment_method_id": good}, "expected_total": micros(9_000_000)})
	require.Equal(t, "order_not_payable", orderError(more))
	require.Len(t, w.nmi.ledger(""), 1, "one charge moved money")
}

// A payment the issuer challenges is requires_action with Stripe's
// authenticate action; after the customer authenticates, confirm reads Stripe
// and the order is paid.
func TestOrderStripeAuthentication(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	member := w.membership("orders:challenged", 12_000_000)
	c := w.newCustomer()
	challenged := c.saveCard("stripe", card{Brand: "visa", Last4: "3155", Decline: "auth"})
	bought := c.order(http.MethodPost, "/orders", "buy-"+uuid.NewString(), map[string]any{"lines": []any{line(member, 0)}, "expected_total": micros(12_000_000), "payment": map[string]any{"payment_method_id": challenged}})
	require.Equal(t, http.StatusCreated, bought.status, "%v", bought.body)
	require.Equal(t, "open", bought.body["status"], "%v", bought.body)
	require.Equal(t, "requires_action", paymentOf(bought)["status"], "%v", bought.body)
	next := paymentOf(bought)["next_action"].(map[string]any)
	require.Equal(t, "authenticate", next["type"])
	payload := next["payload"].(map[string]any)
	require.NotEmpty(t, payload["client_secret"])
	id := bought.body["id"].(string)

	still := c.order(http.MethodPost, "/orders/"+id+"/confirm", "", nil)
	require.Equal(t, "requires_action", paymentOf(still)["status"], "confirm before authenticating changes nothing: %v", still.body)
	require.True(t, w.stripe.Authenticate(payload["payment_intent_id"].(string)))
	confirmed := c.order(http.MethodPost, "/orders/"+id+"/confirm", "", nil)
	require.Equal(t, http.StatusOK, confirmed.status, "%v", confirmed.body)
	require.Equal(t, "complete", confirmed.body["status"], "%v", confirmed.body)
	require.True(t, c.entitled("orders:challenged"))
}

// An order without a payment is open; canceling releases what it claimed,
// expiry closes the next one, and a closed order paid late is revived while
// what it bought is still free, else refunded with a finding.
func TestOrderCancelExpireAndLatePayment(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	life := w.lifetime("orders:late", 7_000_000)
	c := w.newCustomer()

	open := c.order(http.MethodPost, "/orders", "open-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}})
	require.Equal(t, http.StatusCreated, open.status, "%v", open.body)
	require.Equal(t, "open", open.body["status"])
	require.Equal(t, "requires_payment_method", paymentOf(open)["status"])
	require.NotEmpty(t, open.body["payment_options"])
	held := c.order(http.MethodPost, "/orders", "held-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}})
	require.Equal(t, "already_owned", orderError(held), "an unpaid order holds its claim")
	require.Contains(t, errorMeta(held, "owned_by"), billing.OrderIDPrefix)
	require.Equal(t, "resume", errorMeta(held, "hint"))
	canceled := c.order(http.MethodPost, "/orders/"+open.body["id"].(string)+"/cancel", "", nil)
	require.Equal(t, "canceled", canceled.body["status"])
	require.Equal(t, "order_not_cancelable", orderError(c.order(http.MethodPost, "/orders/"+open.body["id"].(string)+"/cancel", "", nil)))

	next := c.order(http.MethodPost, "/orders", "next-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}})
	require.Equal(t, http.StatusCreated, next.status, "the claim was released: %v", next.body)
	w.advance(25 * time.Hour)
	w.expireOrders()
	require.Equal(t, "expired", c.order(http.MethodGet, "/orders/"+next.body["id"].(string), "", nil).body["status"])

	// Revived: the customer completes the challenge at Stripe, never comes
	// back, and cancels; the payment had landed, so the order is paid.
	authCard := c.saveCard("stripe", card{Brand: "visa", Last4: "3155", Decline: "auth"})
	challenged := c.order(http.MethodPost, "/orders", "late-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(7_000_000), "payment": map[string]any{"payment_method_id": authCard}})
	require.Equal(t, "requires_action", paymentOf(challenged)["status"], "%v", challenged.body)
	pi := paymentOf(challenged)["next_action"].(map[string]any)["payload"].(map[string]any)["payment_intent_id"].(string)
	id := challenged.body["id"].(string)
	require.True(t, w.stripe.Authenticate(pi))
	require.Equal(t, http.StatusOK, c.order(http.MethodPost, "/orders/"+id+"/cancel", "", nil).status)
	w.until(func() bool { return c.order(http.MethodGet, "/orders/"+id, "", nil).body["status"] == "complete" }, "the late payment revives the order")
	require.True(t, c.entitled("orders:late"))

	// Refunded: canceling closes the challenged payment at Stripe; the
	// customer buys the product on another order, and the challenge completes
	// before Stripe takes the cancellation.
	d := w.newCustomer()
	dCard := d.saveCard("stripe", card{Brand: "visa", Last4: "3155", Decline: "auth"})
	lost := d.order(http.MethodPost, "/orders", "lost-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(7_000_000), "payment": map[string]any{"payment_method_id": dCard}})
	require.Equal(t, "requires_action", paymentOf(lost)["status"])
	lostPI := paymentOf(lost)["next_action"].(map[string]any)["payload"].(map[string]any)["payment_intent_id"].(string)
	lostID := lost.body["id"].(string)
	g := w.stripe.hold(newGate(func(r *http.Request) bool {
		return r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents/"+lostPI+"/cancel"
	}, false))
	require.Equal(t, "canceled", d.order(http.MethodPost, "/orders/"+lostID+"/cancel", "", nil).body["status"])
	w.advance(time.Hour)
	deadline := time.After(30 * time.Second)
	for arrived := false; !arrived; {
		select {
		case <-g.arrived:
			arrived = true
		case <-deadline:
			t.Fatal("the closed order's payment is canceled at Stripe")
		case <-time.After(100 * time.Millisecond):
			w.promoteOperations()
		}
	}
	nmiCard := d.saveCard("nmi", visa)
	kept := d.order(http.MethodPost, "/orders", "kept-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(7_000_000), "payment": map[string]any{"payment_method_id": nmiCard}})
	require.Equal(t, "complete", kept.body["status"], "%v", kept.body)
	require.True(t, w.stripe.Authenticate(lostPI))
	close(g.release)
	w.until(func() bool { return len(w.openFindings("life.order.late_payment")) == 1 }, "the late payment is refunded with a finding")
	w.stripe.unhold()
	after := d.order(http.MethodGet, "/orders/"+lostID, "", nil)
	require.Equal(t, "canceled", after.body["status"])
	require.Equal(t, "succeeded", paymentOf(after)["status"], "the money moved, and was refunded")
	require.NotNil(t, paymentOf(after)["payment_id"], "the money that moved is recorded")
}

// Two concurrent buys of one product: one order claims it, the other is
// already_owned naming that order.
func TestOrderConcurrentBuys(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	life := w.lifetime("orders:race", 5_000_000)
	c := w.newCustomer()
	var wg sync.WaitGroup
	statuses := make([]int, 4)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{"lines": []any{line(life, 0)}})
			req, _ := http.NewRequest(http.MethodPost, w.server.URL+mountPrefix+"/v1/me/orders", bytes.NewReader(raw))
			req.Header.Set("Authorization", "Bearer "+c.token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "race-"+uuid.NewString())
			res, err := http.DefaultClient.Do(req)
			if err == nil {
				statuses[i] = res.StatusCode
				res.Body.Close()
			}
		}()
	}
	wg.Wait()
	created := 0
	for _, s := range statuses {
		require.Contains(t, []int{http.StatusCreated, http.StatusConflict}, s)
		if s == http.StatusCreated {
			created++
		}
	}
	require.Equal(t, 1, created, "exactly one order claims the product: %v", statuses)
}
