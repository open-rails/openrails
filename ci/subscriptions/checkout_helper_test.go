//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Every purchase is made as production makes it: the merchant creates an
// unpaid order for its customer through its Client, and the customer pays it
// on /v1/me, signed in.

// order is one purchase: what the merchant sells and how the customer pays.
type order struct {
	price  billing.PriceID
	amount *int64
	// rail is the PSP the customer pays on. method is a saved card; token a
	// card the PSP's fields tokenized. With neither the customer pays on the
	// PSP's hosted page (returnURL) or, on Solana, from a wallet.
	rail, method, token string
	returnURL           string
	// ip is the buyer's address as the site's proxy forwards it; empty is a
	// fresh one.
	ip string
	// key is the pay's Idempotency-Key. Empty is one per order and payment,
	// so paying the same way again is a replay, as a page's double submit.
	key string
}

// sale is an order the merchant created for its customer.
type sale struct {
	w     *world
	order billing.Order
}

// paidOrder is a pay's answer read as the payment's status: succeeded,
// processing, requires_action, or failed (declined, Failure says why).
type paidOrder struct {
	Order          billing.Order
	Status         string
	NextAction     *billing.NextAction
	PaymentID      *billing.PaymentID
	SubscriptionID *billing.SubscriptionID
	Failure        *billing.PaymentFailure
	sale           sale
}

func (p *paidOrder) String() string {
	return fmt.Sprintf("%s: order %s %s, failure %+v", p.Status, p.Order.ID, p.Order.Status, p.Failure)
}

// sell is the merchant creating o's order for c through tp's Client.
func (c *customer) sell(tp topology, o order) (sale, error) {
	c.w.t.Helper()
	return c.sellWith(c.w.t.Context(), c.w.client[tp], o)
}

// sellWith is sell through client. It touches no testing.T, so racing
// goroutines call it.
func (c *customer) sellWith(ctx context.Context, client *openrails.Client, o order) (sale, error) {
	created, err := client.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: c.customerID(),
		Lines: []billing.OrderLineParams{{PriceID: o.price, Amount: o.amount}}, IdempotencyKey: uuid.NewString()})
	if err != nil {
		return sale{}, err
	}
	return sale{w: c.w, order: *created}, nil
}

// checkout is c buying o on an order created through tp's Client.
func (c *customer) checkout(tp topology, o order) (*paidOrder, error) {
	c.w.t.Helper()
	s, err := c.sell(tp, o)
	if err != nil {
		return nil, err
	}
	return s.buy(c, o)
}

// mustCheckout is a checkout that succeeds.
func (c *customer) mustCheckout(tp topology, o order) *paidOrder {
	c.w.t.Helper()
	paid, err := c.checkout(tp, o)
	require.NoError(c.w.t, err)
	require.Equal(c.w.t, "succeeded", paid.Status, "%+v", paid.Order)
	c.w.settle()
	return paid
}

// buy is c paying s as o says, on the process that created it.
func (s sale) buy(c *customer, o order) (*paidOrder, error) {
	s.w.t.Helper()
	return s.buyAt(s.w.server.URL, c, o)
}

// buyAt is c paying s on the process server serves: any process of the
// merchant. A processing answer whose charge River's executor is still
// running is the order's outcome once it finishes.
func (s sale) buyAt(server string, c *customer, o order) (*paidOrder, error) {
	s.w.t.Helper()
	paid, err := s.payAt(s.w.t.Context(), server, c, o)
	if err == nil && paid.Status == "processing" && s.awaitPayment() {
		paid = s.readAt(server, c)
	}
	return paid, err
}

// read is the order as its customer reads it.
func (s sale) read(c *customer) *paidOrder {
	s.w.t.Helper()
	return s.readAt(s.w.server.URL, c)
}

func (s sale) readAt(server string, c *customer) *paidOrder {
	s.w.t.Helper()
	status, raw := s.w.pageAt(server, http.MethodGet, "/v1/me/orders/"+s.order.ID.String(), c.token, "", nil)
	require.Equal(s.w.t, http.StatusOK, status, "%s", raw)
	var o billing.Order
	require.NoError(s.w.t, json.Unmarshal(raw, &o))
	return s.answer(o)
}

// executorTimeout bounds how long a helper waits for River's executor to
// finish an operation; one still running then is a hang, and fails the test.
const executorTimeout = 30 * time.Second

// awaitExecutor waits while River's executor runs a provider operation that
// cond (a predicate on billing.provider_intents i) names, and reports whether
// one ran. River can claim an operation before the request that enqueued it
// executes it inline, so the request answers processing meanwhile. An
// operation waiting on evidence (unknown_needs_verify) or a hold is not
// running and answers at once.
func (w *world) awaitExecutor(cond string, args ...any) bool {
	w.t.Helper()
	running := func() bool {
		var found bool
		require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT EXISTS (SELECT 1 FROM billing.provider_intents i WHERE i.status = 'in_flight' AND `+cond+`)`), args...).Scan(&found))
		return found
	}
	if !running() {
		return false
	}
	require.Eventually(w.t, func() bool { return !running() }, executorTimeout, 20*time.Millisecond, "River's executor finishes the operation")
	return true
}

// awaitPayment is awaitExecutor for the order's charge.
func (s sale) awaitPayment() bool {
	s.w.t.Helper()
	return s.w.awaitExecutor(`i.payload->>'order_id' = $1`, s.order.ID.UUID().String())
}

// payment is o's payment as the customer's page sends it.
func (s sale) payment(o order) map[string]any {
	switch {
	case o.method != "":
		return map[string]any{"payment_method_id": o.method}
	case o.token != "":
		return map[string]any{"token": o.token, "psp_id": s.w.psp[o.rail], "billing_details": map[string]any{
			"name": "Page Payer", "address": map[string]any{"postal_code": "10001", "country": "US"}}}
	case o.rail == "solana":
		return map[string]any{"token_symbol": "DUSD"}
	}
	out := map[string]any{"return_url": cmp.Or(o.returnURL, "https://e2e.test/return")}
	if psp, ok := s.w.psp[o.rail]; ok {
		out["psp_id"] = psp
	}
	return out
}

// payAt is c paying s on server as o says. It touches no testing.T, so
// racing goroutines call it. A declined payment (402) is a failed answer.
func (s sale) payAt(ctx context.Context, server string, c *customer, o order) (*paidOrder, error) {
	payment := s.payment(o)
	key := o.key
	if key == "" {
		raw, _ := json.Marshal(payment)
		key = uuid.NewSHA1(uuid.NameSpaceOID, append([]byte(s.order.ID.String()), raw...)).String()
	}
	raw, err := json.Marshal(map[string]any{"payment": payment, "expected_total": strconv.FormatInt(s.order.Total, 10)})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+mountPrefix+"/v1/me/orders/"+s.order.ID.String()+"/pay", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	pageHeaders(req, c.token, o.ip)
	req.Header.Set("Idempotency-Key", key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if raw, err = io.ReadAll(res.Body); err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		err := statusError(res.StatusCode, raw)
		if status, ok := err.(*billing.StatusError); ok && res.StatusCode == http.StatusPaymentRequired && status.Order != nil {
			return s.answer(*status.Order), nil
		}
		return nil, err
	}
	var out billing.Order
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: %s", err, raw)
	}
	return s.answer(out), nil
}

func (s sale) answer(o billing.Order) *paidOrder {
	out := &paidOrder{Order: o, Status: string(o.Payment.Status), NextAction: o.Payment.NextAction, PaymentID: o.Payment.PaymentID, Failure: o.Payment.LastPaymentError, sale: s}
	out.sale.order = o
	if o.Payment.Status == billing.OrderPaymentRequiresPaymentMethod && o.Payment.LastPaymentError != nil {
		out.Status = "failed"
	}
	for _, line := range o.Lines {
		if line.SubscriptionID != nil {
			out.SubscriptionID = line.SubscriptionID
		}
	}
	return out
}

// succeeded is a pay that charged: any other answer is an error.
func succeeded(paid *paidOrder, err error) error {
	if err == nil && paid.Status != "succeeded" {
		err = fmt.Errorf("order payment %v", paid)
	}
	return err
}

var hostedAddress atomic.Int64

// pageHeaders are a page's request: from ip (a fresh address when empty)
// through the site's proxy, with token when the customer is signed in.
func pageHeaders(req *http.Request, token, ip string) {
	req.Header.Set("Content-Type", "application/json")
	if ip == "" {
		ip = fmt.Sprintf("198.51.100.%d", hostedAddress.Add(1)%250+1)
	}
	req.Header.Set("X-Forwarded-For", ip)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// pageAt calls a route on server from ip, with token when the customer is
// signed in there.
func (w *world) pageAt(server, method, path, token, ip string, body any) (int, []byte) {
	w.t.Helper()
	var data io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(w.t, err)
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(w.t.Context(), method, server+mountPrefix+path, data)
	require.NoError(w.t, err)
	pageHeaders(req, token, ip)
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	require.NoError(w.t, err)
	return res.StatusCode, out
}

// statusError is a refusal as the remote Client reads it.
func statusError(status int, raw []byte) error {
	var envelope struct {
		Error *billing.ErrorDetails `json:"error"`
	}
	out := &billing.StatusError{Status: status}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil {
		out.ErrorDetails = *envelope.Error
	}
	return out
}

// attemptID is the checkout attempt the order's latest payment created.
func (s sale) attemptID() billing.CheckoutAttemptID {
	s.w.t.Helper()
	var id uuid.UUID
	require.NoError(s.w.t, s.w.pool.QueryRow(s.w.t.Context(), s.w.q(`SELECT attempt_id FROM billing.orders WHERE id = $1`), s.order.ID.UUID()).Scan(&id))
	return billing.CheckoutAttemptID(id)
}

// attemptStatus is a checkout attempt's stored status.
func (w *world) attemptStatus(id billing.CheckoutAttemptID) string {
	w.t.Helper()
	var status string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT status FROM billing.checkout_attempts WHERE id = $1`), id.UUID()).Scan(&status))
	return status
}

// latestAttemptStatus is the status of the customer's newest order payment attempt.
func (w *world) latestAttemptStatus(customerID string) string {
	w.t.Helper()
	var status string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT status FROM billing.checkout_attempts WHERE customer_id = $1 AND order_id IS NOT NULL ORDER BY created_at DESC, id DESC LIMIT 1`), customerID).Scan(&status))
	return status
}
