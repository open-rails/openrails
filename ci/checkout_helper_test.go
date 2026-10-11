//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

// A purchase is made as production makes it: the merchant creates an unpaid
// order for its customer through its Client, and the customer pays it on
// /v1/me, signed in.

// sale is what the merchant sells: one price to one customer.
type sale struct {
	customer billing.CustomerID
	price    billing.PriceID
	amount   *int64
}

type purchase struct {
	t     *testing.T
	page  http.Handler
	token string
	order billing.Order
	// addr is the customer's client address. Redis-backed abuse limits count
	// per address and outlive a test, so each customer has its own.
	addr string
}

// sell creates s's order through client and opens it as its customer on
// bill's mount.
func sell(t *testing.T, bill *openrails.Engine, client *openrails.Client, s sale) (*purchase, error) {
	t.Helper()
	order, err := client.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: s.customer,
		Lines: []billing.OrderLineParams{{PriceID: s.price, Amount: s.amount}}, IdempotencyKey: uuid.NewString()})
	if err != nil {
		return nil, err
	}
	return (&purchase{t: t, order: *order, addr: freshAddr()}).on(bill), nil
}

// on is the purchase on bill's mount: any process of its merchant serves
// its customer.
func (p *purchase) on(bill *openrails.Engine) *purchase {
	p.t.Helper()
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(p.t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth}))
	return &purchase{t: p.t, page: mux, token: auth.Person(p.order.CustomerID.String()), order: p.order, addr: p.addr}
}

func (p *purchase) call(method, path, key string, body any) (int, []byte) {
	p.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(p.t, err)
	}
	req := httptest.NewRequest(method, "/v1/me/orders/"+p.order.ID.String()+path, bytes.NewReader(raw))
	req.RemoteAddr = p.addr
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	p.page.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// read is the order as its customer reads it.
func (p *purchase) read() billing.Order {
	p.t.Helper()
	status, raw := p.call(http.MethodGet, "", "", nil)
	require.Equal(p.t, http.StatusOK, status, "%s", raw)
	var out billing.Order
	require.NoError(p.t, json.Unmarshal(raw, &out))
	return out
}

// pay pays the order under key with payment: a redirect PSP's hosted page
// (return_url), a Solana wallet (token_symbol) or a card. A refusal, a
// declined payment's 402 included, is the error the remote Client reads.
func (p *purchase) pay(key string, payment map[string]any) (*billing.Order, error) {
	p.t.Helper()
	status, raw := p.call(http.MethodPost, "/pay", key, map[string]any{"payment": payment, "expected_total": strconv.FormatInt(p.order.Total, 10)})
	if status != http.StatusOK {
		var envelope struct {
			Error *billing.ErrorDetails `json:"error"`
		}
		out := &billing.StatusError{Status: status}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil {
			out.ErrorDetails = *envelope.Error
		}
		return nil, out
	}
	var out billing.Order
	require.NoError(p.t, json.Unmarshal(raw, &out))
	return &out, nil
}

// redirect is a payment on a redirect PSP's hosted page, returning to url.
func redirect(url string) map[string]any { return map[string]any{"return_url": url} }

// attempt is the checkout attempt the order's payment created.
func (p *purchase) attempt(f *fixture) billing.CheckoutAttemptID {
	p.t.Helper()
	var id uuid.UUID
	require.NoError(p.t, f.pool.QueryRow(p.t.Context(), "SELECT attempt_id FROM "+pgx.Identifier{f.schema, "orders"}.Sanitize()+" WHERE id = $1", p.order.ID.UUID()).Scan(&id))
	return billing.CheckoutAttemptID(id)
}

// requireCode is a refusal with code.
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var status *billing.StatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, code, status.Code, "%v", err)
}
