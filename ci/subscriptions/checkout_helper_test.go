//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
)

// Every purchase is made as production makes it: the merchant hands its
// customer a checkout session through its Client, and the payment page pays
// it by its id, the customer signed in to pay with a saved card.

// order is one purchase: what the session sells and how the page pays it.
type order struct {
	price      billing.PriceID
	amount     *int64
	autoRenew  *bool
	successURL string
	// rail is the option the page pays on. method is a saved card, paid with
	// the customer's own proof; token a card the page tokenized. With neither
	// the option pays by redirect or Solana Pay.
	rail, method, token string
	// ip is the buyer's address as the site's proxy forwards it; empty is a
	// fresh one.
	ip string
}

// sessionPaid is the page's answer to a pay and the session it paid.
type sessionPaid struct {
	checkoutsession.CheckoutSessionPayResult
	session hostedSession
}

// sell is the merchant minting o's session for c through tp's Client.
func (c *customer) sell(tp topology, o order) (hostedSession, error) {
	c.w.t.Helper()
	link, err := c.w.client[tp].CreateCheckoutSession(c.w.t.Context(), billing.CreateCheckoutSessionParams{
		Customer: c.identity(), PriceID: o.price, Amount: o.amount, AutoRenew: o.autoRenew, SuccessURL: o.successURL,
	})
	if err != nil {
		return hostedSession{}, err
	}
	return hostedSession{w: c.w, id: link.ID}, nil
}

// checkout is c buying o on a session minted through tp's Client.
func (c *customer) checkout(tp topology, o order) (*sessionPaid, error) {
	c.w.t.Helper()
	s, err := c.sell(tp, o)
	if err != nil {
		return nil, err
	}
	return s.buy(c, o)
}

// mustCheckout is a checkout that succeeds.
func (c *customer) mustCheckout(tp topology, o order) *sessionPaid {
	c.w.t.Helper()
	paid, err := c.checkout(tp, o)
	require.NoError(c.w.t, err)
	require.Equal(c.w.t, "succeeded", paid.Status, "%+v", paid.CheckoutSessionPayResult)
	c.w.settle()
	return paid
}

// buy is c paying s as o says, on the page of the process that minted it.
func (s hostedSession) buy(c *customer, o order) (*sessionPaid, error) {
	s.w.t.Helper()
	return s.buyAt(s.w.server.URL, c, o)
}

// buyAt is c paying s on the page server serves: any process of the merchant.
func (s hostedSession) buyAt(server string, c *customer, o order) (*sessionPaid, error) {
	s.w.t.Helper()
	return s.payAt(s.w.t.Context(), server, s.optionAt(server, o.rail), c, o)
}

// payAt is c paying option of s on server's page as o says. It touches no
// testing.T, so racing goroutines call it.
func (s hostedSession) payAt(ctx context.Context, server, option string, c *customer, o order) (*sessionPaid, error) {
	body := map[string]any{"option_id": option}
	token := ""
	switch {
	case o.method != "":
		body["payment_method_id"], token = o.method, c.token
	case o.token != "":
		body["payment_token"] = o.token
		body["billing_details"] = map[string]any{"name": "Page Payer", "address": map[string]any{"postal_code": "10001", "country": "US"}}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+mountPrefix+"/v1/checkout-sessions/"+s.id+"/pay", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	pageHeaders(req, token, o.ip)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if raw, err = io.ReadAll(res.Body); err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, statusError(res.StatusCode, raw)
	}
	out := &sessionPaid{session: s}
	if err := json.Unmarshal(raw, &out.CheckoutSessionPayResult); err != nil {
		return nil, fmt.Errorf("%w: %s", err, raw)
	}
	return out, nil
}

// succeeded is a pay that charged: any other answer is an error.
func succeeded(paid *sessionPaid, err error) error {
	if err == nil && paid.Status != "succeeded" {
		err = fmt.Errorf("checkout %s: %+v", paid.Status, paid.CheckoutSessionPayResult)
	}
	return err
}

// pageHeaders are a payment page request's: from ip (a fresh address when
// empty) through the site's proxy, with token when the customer is signed in.
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

// optionAt is the session's option on rail, as server's page reads it.
func (s hostedSession) optionAt(server, rail string) string {
	s.w.t.Helper()
	option, err := s.optionOf(s.w.t.Context(), server, rail)
	require.NoError(s.w.t, err)
	return option
}

// optionOf is optionAt for racing goroutines.
func (s hostedSession) optionOf(ctx context.Context, server, rail string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+mountPrefix+"/v1/checkout-sessions/"+s.id, nil)
	if err != nil {
		return "", err
	}
	pageHeaders(req, "", "")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", statusError(res.StatusCode, raw)
	}
	var doc checkoutsession.CheckoutSession
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	for _, option := range doc.Options {
		if option.Rail == rail {
			return option.ID, nil
		}
	}
	return "", fmt.Errorf("no %s option: %s", rail, raw)
}

// pageAt calls a payment page route on server from ip, with token when the
// customer is signed in there.
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

// attemptID is the checkout attempt the session's current payment created.
func (s hostedSession) attemptID() billing.CheckoutAttemptID {
	s.w.t.Helper()
	var id uuid.UUID
	require.NoError(s.w.t, s.w.pool.QueryRow(s.w.t.Context(), s.w.q(`SELECT attempt_id FROM billing.checkout_sessions WHERE id_hash = sha256($1::bytea)`), []byte(s.id)).Scan(&id))
	return billing.CheckoutAttemptID(id)
}

// attemptStatus is a checkout attempt's stored status.
func (w *world) attemptStatus(id billing.CheckoutAttemptID) string {
	w.t.Helper()
	var status string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT status FROM billing.checkout_attempts WHERE id = $1`), id.UUID()).Scan(&status))
	return status
}

// latestAttemptStatus is the status of the customer's newest purchase attempt
// (card setups excluded: they share the frozen clock's timestamp).
func (w *world) latestAttemptStatus(customerID string) string {
	w.t.Helper()
	var status string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT status FROM billing.checkout_attempts WHERE customer_id = $1 AND mode <> 'payment_method' ORDER BY created_at DESC, id DESC LIMIT 1`), customerID).Scan(&status))
	return status
}

// The engine's own checkout entry, for inputs no checkout session sends: a
// wallet-connected Solana subscribe, a one-off Solana transaction request, a
// signature relayed from the buyer's wallet, an asserted entitlement. No
// production route reaches them since the merchant checkout-attempt routes
// were removed; these scenarios keep them covered until they are deleted.

func (w *world) inMerchant(run func(ctx context.Context, svc *checkout.CheckoutAttemptService) error) error {
	rt := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(w.t.Context(), rt.ConfiguredMerchant())
	return rt.DB.RunInMerchantConn(ctx, func(scoped context.Context) error { return run(scoped, rt.CheckoutAttemptService) })
}

// engineCheckout creates an attempt for c straight on the engine.
func (w *world) engineCheckout(c *customer, in checkout.CheckoutAttemptCreateRequest) (*checkout.CheckoutAttemptResponse, error) {
	w.t.Helper()
	if in.IdempotencyKey == "" {
		in.IdempotencyKey = "engine-" + uuid.NewString()
	}
	// The payer accepts the price's terms in this request, as a session's pay does.
	in.Acceptance = &billingauth.Payer{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: engine.Graph(w.rt).Runtime.ConfiguredMerchant(), SubjectID: c.id, Issuer: "openrails:merchant-checkout"}
	var out *checkout.CheckoutAttemptResponse
	err := w.inMerchant(func(ctx context.Context, svc *checkout.CheckoutAttemptService) (err error) {
		out, err = svc.CreateSession(ctx, &in, &checkout.UserIdentity{ID: c.id})
		return err
	})
	return out, err
}

// engineConfirm relays a Solana signature for the attempt to the engine, as
// its buyer's wallet sent it.
func (w *world) engineConfirm(id billing.CheckoutAttemptID, signature string) (*checkout.CheckoutAttemptResponse, error) {
	w.t.Helper()
	var out *checkout.CheckoutAttemptResponse
	err := w.inMerchant(func(ctx context.Context, svc *checkout.CheckoutAttemptService) error {
		owner, err := svc.Owner(ctx, id.UUID())
		if err != nil {
			return err
		}
		in := &checkout.CheckoutAttemptConfirmRequest{Payment: checkout.CheckoutAttemptConfirmPayment{Rail: owner.Rail, Signature: signature}}
		out, err = svc.ConfirmSession(ctx, id.UUID(), in, &checkout.UserIdentity{ID: owner.CustomerID.String()})
		return err
	})
	return out, err
}
