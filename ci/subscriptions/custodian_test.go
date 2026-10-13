//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/nmimock"
)

// btFake is a Basis Theory tenant: the token intents a browser collected, the
// tokens they convert to, and the detokenizing proxy, which hands each sale
// to the NMI gateway behind it and records its credential-on-file fields.
type btFake struct {
	mu                     sync.Mutex
	seq                    int
	intents                map[string]string // id -> card number
	applications           map[string]string // token intent -> creating application
	tokens                 map[string]string
	byKey                  map[string]string // idempotency key -> token
	sales                  []btSale
	tenant, application    string
	containers             map[string][]string
	decline                bool
	failConversion         bool
	loseConversionResponse bool
	loseSaleResponse       bool
	beforeTokenRead        func()
	// gateway charges the detokenized card and answers its transaction id.
	gateway func(form url.Values, number string) string
}

type btSale struct {
	TransactionID, Number                string
	InitiatedBy, Indicator, InitialTxnID string
	// SecurityKey names the gateway account the proxy posted to.
	SecurityKey string
	CVV, Source string
}

func newBTFake(t *testing.T) (*btFake, string) {
	f := &btFake{intents: map[string]string{}, tokens: map[string]string{}, byKey: map[string]string{}, containers: map[string][]string{}, applications: map[string]string{}}
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
	f.applications[id] = f.application
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
		card := btCard(id, number)
		card["tenant_id"], card["created_by"] = f.tenant, f.applications[id]
		reply(http.StatusOK, card)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/tokens/"):
		id := strings.TrimPrefix(r.URL.Path, "/tokens/")
		number, ok := f.tokens[id]
		if !ok {
			reply(http.StatusNotFound, map[string]any{"title": "not found"})
			return
		}
		if f.beforeTokenRead != nil {
			f.beforeTokenRead()
			f.beforeTokenRead = nil
		}
		card := btCard(id, number)
		card["tenant_id"], card["containers"] = f.tenant, f.containers[id]
		reply(http.StatusOK, card)
	case r.Method == http.MethodPost && r.URL.Path == "/tokens":
		var req struct {
			IntentID    string   `json:"token_intent_id"`
			Deduplicate bool     `json:"deduplicate_token"`
			Containers  []string `json:"containers"`
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
		if f.failConversion {
			reply(http.StatusServiceUnavailable, map[string]any{"title": "conversion temporarily unavailable"})
			return
		}
		card := func(id string) map[string]any {
			c := btCard(id, number)
			c["tenant_id"], c["containers"] = f.tenant, f.containers[id]
			return c
		}
		key := r.Header.Get("BT-IDEMPOTENCY-KEY")
		if id, ok := f.byKey[key]; ok && key != "" {
			reply(http.StatusCreated, card(id))
			return
		}
		if req.Deduplicate {
			for id, held := range f.tokens {
				if held == number && strings.Join(f.containers[id], ",") == strings.Join(req.Containers, ",") {
					reply(http.StatusCreated, card(id))
					return
				}
			}
		}
		f.seq++
		id := fmt.Sprintf("tok_%d", f.seq)
		f.tokens[id], f.byKey[key], f.containers[id] = number, id, req.Containers
		if f.loseConversionResponse {
			f.loseConversionResponse = false
			reply(http.StatusServiceUnavailable, map[string]any{"title": "conversion response lost"})
			return
		}
		reply(http.StatusCreated, card(id))
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
		if f.decline {
			f.sales = append(f.sales, btSale{Number: number})
			w.Header().Set("BT-PROXY-DESTINATION-STATUS", "200")
			_, _ = w.Write([]byte("response=2&responsetext=DECLINED&response_code=200"))
			return
		}
		sale := btSale{TransactionID: f.gateway(r.PostForm, number), Number: number, InitiatedBy: r.PostForm.Get("initiated_by"),
			Indicator: r.PostForm.Get("stored_credential_indicator"), InitialTxnID: r.PostForm.Get("initial_transaction_id"), SecurityKey: r.PostForm.Get("security_key"), CVV: r.PostForm.Get("cvv"), Source: m[1]}
		f.sales = append(f.sales, sale)
		if f.loseSaleResponse {
			f.loseSaleResponse = false
			reply(http.StatusBadGateway, map[string]any{"title": "response lost after forwarding"})
			return
		}
		w.Header().Set("BT-PROXY-DESTINATION-STATUS", "200")
		_, _ = w.Write([]byte(url.Values{"response": {"1"}, "responsetext": {"SUCCESS"}, "authcode": {"123456"}, "transactionid": {sale.TransactionID},
			"avsresponse": {""}, "cvvresponse": {""}, "orderid": {r.PostForm.Get("orderid")}, "type": {"sale"}, "response_code": {"100"}}.Encode()))
	default:
		reply(http.StatusNotFound, map[string]any{"title": r.Method + " " + r.URL.Path})
	}
}

// custodianMethod is a customer's Basis Theory card: its token and its
// card-on-file agreement.
type custodianMethod struct{ Token, Unscheduled string }

func (w *world) custodianMethods(c *customer) []custodianMethod {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT pm.rail_method_ref, COALESCE((SELECT m.initial_transaction_id FROM billing.mandates m
		WHERE m.merchant_id = pm.merchant_id AND m.payment_method_id = pm.id AND m.kind = 'card_on_file' AND m.status = 'active'), '')
		FROM billing.payment_methods pm WHERE pm.customer_id = $1 AND pm.custodian = 'basis_theory' ORDER BY pm.created_at, pm.id`), c.id)
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
		psps["nmi-bt"] = openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, AccountID: "e2e-nmi-bt", SecurityKey: "e2e-nmi-bt-key", WebhookSigningSecret: "nmi_bt_webhook_e2e", Custodian: "bt"}.PSPConfig()
	}
	// The merchant sells cards through its custodian's PSP.
	routing := []billing.PSPRoutingRule{{Prefer: []string{"nmi-bt"}}}
	w.settings = billing.MerchantSettings{PSPRouting: &routing}
	w.start()
	// The gateway takes the card number through the proxy; the mock files
	// those sales under one keyed-card record.
	keyed := w.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
	bt.gateway = func(form url.Values, number string) string {
		return w.nmi.AddSale(nmimock.Sale{Vault: keyed, OrderID: form.Get("orderid"), Amount: form.Get("amount"),
			InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator"), Initial: form.Get("initial_transaction_id")}).TransactionID
	}
	rt := engine.Graph(w.rt).Runtime
	rt.PurchaseService.CustodianSaleService.BTBaseURLOverride = btURL

	// The page's Basis Theory Elements collect the card; its token intent
	// pays the order on the custodian's PSP. The order names the card it
	// kept and the proxy presented.
	buy := func(c *customer, price billing.PriceID, number string) (card, token string) {
		t.Helper()
		paid := c.mustPurchase(embedded, order{price: price, rail: "nmi-bt", token: bt.collect(number)})
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT COALESCE(pm.rail_method_ref, ''), COALESCE(p.token_type, '') FROM billing.orders o
			JOIN billing.payments p ON p.merchant_id = o.merchant_id AND p.id = o.payment_id
			LEFT JOIN billing.payment_methods pm ON pm.merchant_id = o.merchant_id AND pm.id = o.payment_method_id WHERE o.id = $1`), paid.Order.ID.UUID()).Scan(&card, &token))
		require.Equal(t, "pan_via_proxy", token)
		return card, token
	}
	const number = "4111111111111111"
	first, second := w.permanent("content:first"), w.permanent("content:second")
	alice, bob := w.newCustomer(), w.newCustomer()

	aliceFirst, _ := buy(alice, first.ID, number)
	bobFirst, _ := buy(bob, first.ID, number)
	sales := bt.salesOf(number)
	require.Len(t, sales, 2)
	for i, sale := range sales {
		require.Equal(t, []string{"customer", "stored", ""}, []string{sale.InitiatedBy, sale.Indicator, sale.InitialTxnID}, "sale %d starts its own customer's agreement", i)
	}
	aliceCard, bobCard := w.custodianMethods(alice), w.custodianMethods(bob)
	require.Equal(t, []custodianMethod{{Token: aliceCard[0].Token, Unscheduled: sales[0].TransactionID}}, aliceCard)
	require.Equal(t, []custodianMethod{{Token: bobCard[0].Token, Unscheduled: sales[1].TransactionID}}, bobCard)
	require.NotEqual(t, aliceCard[0].Token, bobCard[0].Token, "each customer's card is its own token")
	require.Equal(t, []string{aliceCard[0].Token, bobCard[0].Token}, []string{aliceFirst, bobFirst}, "each order names the card it kept")

	aliceSecond, _ := buy(alice, second.ID, number)
	bobSecond, _ := buy(bob, second.ID, number)
	require.Equal(t, []string{aliceFirst, bobFirst}, []string{aliceSecond, bobSecond}, "a returning card pays as the customer's own")
	sales = bt.salesOf(number)
	require.Len(t, sales, 4)
	require.Equal(t, []string{"customer", "used", sales[0].TransactionID}, []string{sales[2].InitiatedBy, sales[2].Indicator, sales[2].InitialTxnID}, "alice's charge names her agreement")
	require.Equal(t, []string{"customer", "used", sales[1].TransactionID}, []string{sales[3].InitiatedBy, sales[3].Indicator, sales[3].InitialTxnID}, "bob's charge names his")
	require.Equal(t, aliceCard, w.custodianMethods(alice), "a returning customer keeps one instrument")
	require.Equal(t, bobCard, w.custodianMethods(bob))
	require.True(t, alice.entitled("content:second"))
	require.True(t, bob.entitled("content:second"))
}
