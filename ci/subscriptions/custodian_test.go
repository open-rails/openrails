//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/nmimock"
)

// btFake is a Basis Theory tenant: the token intents a browser collected, the
// tokens they convert to, and the detokenizing proxy, which hands each sale
// to the NMI gateway behind it and records its credential-on-file fields.
type btFake struct {
	mu      sync.Mutex
	seq     int
	intents map[string]string // id -> card number
	tokens  map[string]string
	byKey   map[string]string // idempotency key -> token
	sales   []btSale
	// gateway charges the detokenized card and answers its transaction id.
	gateway func(form url.Values, number string) string
}

type btSale struct {
	TransactionID, Number                string
	InitiatedBy, Indicator, InitialTxnID string
}

func newBTFake(t *testing.T) (*btFake, string) {
	f := &btFake{intents: map[string]string{}, tokens: map[string]string{}, byKey: map[string]string{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return f, server.URL
}

// collect is a browser collecting a card into a token intent.
func (f *btFake) collect(number string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("ti_%d", f.seq)
	f.intents[id] = number
	return id
}

func (f *btFake) salesOf(number string) []btSale {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []btSale
	for _, s := range f.sales {
		if s.Number == number {
			out = append(out, s)
		}
	}
	return out
}

// The fingerprint is the card number's, whoever collected it.
func btCard(id, number string) map[string]any {
	return map[string]any{"id": id, "type": "card", "fingerprint": "fp_" + number,
		"card": map[string]any{"bin": number[:6], "last4": number[len(number)-4:], "brand": "visa", "expiration_month": 12, "expiration_year": 2035}}
}

var btExpression = regexp.MustCompile(`\{\{ (token_intent|token): (\S+) \|`)

func (f *btFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/token-intents/"):
		id := strings.TrimPrefix(r.URL.Path, "/token-intents/")
		number, ok := f.intents[id]
		if !ok {
			reply(http.StatusNotFound, map[string]any{"title": "not found"})
			return
		}
		reply(http.StatusOK, btCard(id, number))
	case r.Method == http.MethodPost && r.URL.Path == "/tokens":
		var req struct {
			IntentID    string `json:"token_intent_id"`
			Deduplicate bool   `json:"deduplicate_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(http.StatusBadRequest, map[string]any{"title": err.Error()})
			return
		}
		number, ok := f.intents[req.IntentID]
		if !ok {
			reply(http.StatusNotFound, map[string]any{"title": "not found"})
			return
		}
		key := r.Header.Get("BT-IDEMPOTENCY-KEY")
		if id, ok := f.byKey[key]; ok && key != "" {
			reply(http.StatusCreated, btCard(id, number))
			return
		}
		if req.Deduplicate {
			for id, held := range f.tokens {
				if held == number {
					reply(http.StatusCreated, btCard(id, number))
					return
				}
			}
		}
		f.seq++
		id := fmt.Sprintf("tok_%d", f.seq)
		f.tokens[id], f.byKey[key] = number, id
		reply(http.StatusCreated, btCard(id, number))
	case r.Method == http.MethodPost && r.URL.Path == "/proxy":
		if err := r.ParseForm(); err != nil {
			reply(http.StatusBadRequest, map[string]any{"proxy_error": map[string]any{"title": err.Error()}})
			return
		}
		m := btExpression.FindStringSubmatch(r.PostForm.Get("ccnumber"))
		held := f.tokens
		if m != nil && m[1] == "token_intent" {
			held = f.intents
		}
		number, ok := "", false
		if m != nil {
			number, ok = held[m[2]]
		}
		if !ok {
			reply(http.StatusBadRequest, map[string]any{"proxy_error": map[string]any{"title": "detokenization failed"}})
			return
		}
		sale := btSale{TransactionID: f.gateway(r.PostForm, number), Number: number, InitiatedBy: r.PostForm.Get("initiated_by"),
			Indicator: r.PostForm.Get("stored_credential_indicator"), InitialTxnID: r.PostForm.Get("initial_transaction_id")}
		f.sales = append(f.sales, sale)
		w.Header().Set("BT-PROXY-DESTINATION-STATUS", "200")
		_, _ = w.Write([]byte(url.Values{"response": {"1"}, "responsetext": {"SUCCESS"}, "authcode": {"123456"}, "transactionid": {sale.TransactionID},
			"avsresponse": {""}, "cvvresponse": {""}, "orderid": {r.PostForm.Get("orderid")}, "type": {"sale"}, "response_code": {"100"}}.Encode()))
	default:
		reply(http.StatusNotFound, map[string]any{"title": r.Method + " " + r.URL.Path})
	}
}

// custodianMethod is a customer's Basis Theory card: its token and its
// unscheduled agreement.
type custodianMethod struct{ Token, Unscheduled string }

func (w *world) custodianMethods(c *customer) []custodianMethod {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT rail_method_ref, COALESCE(stored_credential_unscheduled_ref, '') FROM billing.payment_methods
		WHERE customer_id = $1 AND custodian = 'basis_theory' ORDER BY created_at, id`), c.id)
	require.NoError(w.t, err)
	defer rows.Close()
	var out []custodianMethod
	for rows.Next() {
		var m custodianMethod
		require.NoError(w.t, rows.Scan(&m.Token, &m.Unscheduled))
		out = append(out, m)
	}
	require.NoError(w.t, rows.Err())
	return out
}

// A custodian fingerprints the card number, not the customer. Two customers
// who pay with the same card each get their own instrument, and each later
// charge names only its own customer's agreement, never the other's.
func TestCustodianSaleAnchorsOnlyOnItsOwnCustomer(t *testing.T) {
	t.Parallel()
	bt, btURL := newBTFake(t)
	w := prepareWorld(t, 12)
	w.custodians = map[string]openrails.CustodianConfig{"bt": {Kind: "basis_theory", AccountID: "bt-e2e",
		Settings: map[string]any{"public_api_key": "key_public_e2e"}, Secrets: map[string]string{"api_key": "key_private_e2e"}}}
	w.declare = func(psps map[string]openrails.PSPConfig) {
		psps["nmi-bt"] = openrails.NMIPSP{AccountID: "e2e-nmi-bt", SecurityKey: "e2e-nmi-bt-key", WebhookSigningSecret: "nmi_bt_webhook_e2e", Custodian: "bt"}.PSPConfig()
	}
	w.start()
	// The gateway takes the card number through the proxy; the mock files
	// those sales under one keyed-card record.
	keyed := w.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
	bt.gateway = func(form url.Values, number string) string {
		return w.nmi.AddSale(nmimock.Sale{Vault: keyed, OrderID: form.Get("orderid"), Amount: form.Get("amount"),
			InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator"), Initial: form.Get("initial_transaction_id")}).TransactionID
	}
	rt := engine.Graph(w.rt).Runtime
	rt.CheckoutService.CustodianSaleService.BTBaseURLOverride = btURL

	buy := func(c *customer, price billing.PriceID, number string) {
		t.Helper()
		ctx := merchant.WithID(t.Context(), rt.ConfiguredMerchant())
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			res, err := rt.CheckoutService.Checkout(ctx, &checkout.CheckoutRequest{PriceID: price.String(), Rail: "nmi-bt", BTTokenIntentID: bt.collect(number), IdempotencyKey: uuid.NewString()}, &checkout.UserIdentity{ID: c.id})
			if err != nil {
				return err
			}
			if res.Status != "success" {
				return fmt.Errorf("checkout answered %+v", res)
			}
			return nil
		}))
		w.settle()
	}
	const number = "4111111111111111"
	first, second := w.permanent("content:first"), w.permanent("content:second")
	alice, bob := w.newCustomer(), w.newCustomer()

	buy(alice, first.ID, number)
	buy(bob, first.ID, number)
	sales := bt.salesOf(number)
	require.Len(t, sales, 2)
	for i, sale := range sales {
		require.Equal(t, []string{"customer", "stored", ""}, []string{sale.InitiatedBy, sale.Indicator, sale.InitialTxnID}, "sale %d starts its own customer's agreement", i)
	}
	aliceCard, bobCard := w.custodianMethods(alice), w.custodianMethods(bob)
	require.Equal(t, []custodianMethod{{Token: aliceCard[0].Token, Unscheduled: sales[0].TransactionID}}, aliceCard)
	require.Equal(t, []custodianMethod{{Token: bobCard[0].Token, Unscheduled: sales[1].TransactionID}}, bobCard)
	require.NotEqual(t, aliceCard[0].Token, bobCard[0].Token, "each customer's card is its own token")

	buy(alice, second.ID, number)
	buy(bob, second.ID, number)
	sales = bt.salesOf(number)
	require.Len(t, sales, 4)
	require.Equal(t, []string{"customer", "used", sales[0].TransactionID}, []string{sales[2].InitiatedBy, sales[2].Indicator, sales[2].InitialTxnID}, "alice's charge names her agreement")
	require.Equal(t, []string{"customer", "used", sales[1].TransactionID}, []string{sales[3].InitiatedBy, sales[3].Indicator, sales[3].InitialTxnID}, "bob's charge names his")
	require.Equal(t, aliceCard, w.custodianMethods(alice), "a returning customer keeps one instrument")
	require.Equal(t, bobCard, w.custodianMethods(bob))
	require.True(t, alice.entitled("content:second"))
	require.True(t, bob.entitled("content:second"))
}
