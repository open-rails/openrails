//go:build greenfield && integration

package subscriptions_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// card is what a browser tokenizes. Decline is "" (approve), a Stripe
// decline_code / NMI response_code, or "auth" (issuer authentication).
type card struct {
	Brand, Last4 string
	Decline      string
}

var (
	visa       = card{Brand: "visa", Last4: "4242"}
	mastercard = card{Brand: "mastercard", Last4: "4444"}
)

type obj = map[string]any

// providerCall is one journaled provider mutation.
type providerCall struct {
	Method, Path string
	Form         url.Values
}

// gate parks one matching provider request until released or its caller
// abandons it. commit decides whether an abandoned request still took
// effect at the provider (commit-then-loss) or never arrived.
type gate struct {
	match   func(*http.Request) bool
	arrived chan struct{}
	release chan struct{}
	commit  bool
	once    sync.Once
}

func newGate(match func(*http.Request) bool, commit bool) *gate {
	return &gate{match: match, arrived: make(chan struct{}), release: make(chan struct{}), commit: commit}
}

// stripeFake is a stateful Stripe API: customers, setup intents, payment
// methods, payment intents with charges, refunds and provider-owned
// subscriptions. Every mutation is journaled; idempotency keys replay.
type stripeFake struct {
	mu        sync.Mutex
	seq       int
	customers map[string]obj
	setups    map[string]obj
	methods   map[string]obj
	declines  map[string]string
	intents   map[string]obj
	order     []string
	charges   map[string]obj
	refunds   map[string]obj
	subs      map[string]obj
	idem      map[string][]byte
	writes    []providerCall
	gates     []*gate
	odd       []string
}

func newStripeFake() *stripeFake {
	return &stripeFake{customers: map[string]obj{}, setups: map[string]obj{}, methods: map[string]obj{}, declines: map[string]string{},
		intents: map[string]obj{}, charges: map[string]obj{}, refunds: map[string]obj{}, subs: map[string]obj{}, idem: map[string][]byte{}}
}

func (f *stripeFake) id(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_gf%06d", prefix, f.seq)
}

func (f *stripeFake) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	form, _ := url.ParseQuery(string(body))
	f.mu.Lock()
	var g *gate
	for _, candidate := range f.gates {
		if candidate.match(r) {
			g = candidate
		}
	}
	f.mu.Unlock()
	if g != nil {
		g.once.Do(func() { close(g.arrived) })
		select {
		case <-g.release:
		case <-r.Context().Done():
			if g.commit {
				f.serve(r, form)
			}
			return nil, r.Context().Err()
		}
	}
	rec := f.serve(r, form)
	res := rec.Result()
	res.Request = r
	return res, nil
}

func (f *stripeFake) hold(g *gate) *gate {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates = append(f.gates, g)
	return g
}

func (f *stripeFake) unhold() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates = nil
}

func (f *stripeFake) serve(r *http.Request, form url.Values) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Header.Get("Idempotency-Key")
	if r.Method != http.MethodGet && key != "" {
		if cached, ok := f.idem[r.URL.Path+"|"+key]; ok {
			_, _ = rec.Write(cached)
			return rec
		}
	}
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, providerCall{Method: r.Method, Path: r.URL.Path, Form: form})
	}
	status, out := f.route(r, form)
	raw, _ := json.Marshal(out)
	if status == http.StatusOK && r.Method != http.MethodGet && key != "" {
		f.idem[r.URL.Path+"|"+key] = raw
	}
	rec.WriteHeader(status)
	_, _ = rec.Write(raw)
	return rec
}

func metadataOf(form url.Values) map[string]string {
	out := map[string]string{}
	for k, vs := range form {
		if strings.HasPrefix(k, "metadata[") {
			out[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = vs[0]
		}
	}
	return out
}

var metadataQuery = regexp.MustCompile(`metadata\['([^']+)'\]:'([^']*)'`)

func (f *stripeFake) route(r *http.Request, form url.Values) (int, any) {
	p, q := r.URL.Path, r.URL.Query()
	seg := strings.Split(strings.TrimPrefix(p, "/v1/"), "/")
	switch {
	case p == "/v1/account":
		return 200, obj{"object": "account", "id": stripeAcct, "charges_enabled": true}
	case p == "/v1/balance":
		return 200, obj{"object": "balance", "livemode": false, "available": []any{}, "pending": []any{}}
	case r.Method == http.MethodGet && p == "/v1/customers/search":
		var data []any
		for _, c := range f.customers {
			match := true
			for _, m := range metadataQuery.FindAllStringSubmatch(q.Get("query"), -1) {
				if c["metadata"].(map[string]string)[m[1]] != m[2] {
					match = false
				}
			}
			if match {
				data = append(data, c)
			}
		}
		return 200, obj{"object": "search_result", "data": data, "has_more": false}
	case r.Method == http.MethodPost && p == "/v1/customers":
		c := obj{"object": "customer", "id": f.id("cus"), "email": form.Get("email"), "metadata": metadataOf(form), "livemode": false}
		f.customers[c["id"].(string)] = c
		return 200, c
	case r.Method == http.MethodGet && seg[0] == "customers" && len(seg) == 2:
		if c, ok := f.customers[seg[1]]; ok {
			return 200, c
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodPost && p == "/v1/setup_intents":
		s := obj{"object": "setup_intent", "id": f.id("seti"), "status": "requires_payment_method", "customer": form.Get("customer"), "usage": form.Get("usage"),
			"payment_method_types": []string{"card"}, "livemode": false, "metadata": metadataOf(form)}
		s["client_secret"] = s["id"].(string) + "_secret_gf"
		f.setups[s["id"].(string)] = s
		return 200, s
	case r.Method == http.MethodGet && seg[0] == "setup_intents" && len(seg) == 2:
		if s, ok := f.setups[seg[1]]; ok {
			return 200, s
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodGet && seg[0] == "payment_methods" && len(seg) == 2:
		if m, ok := f.methods[seg[1]]; ok {
			return 200, m
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodPost && p == "/v1/payment_intents":
		return f.createIntent(form)
	case r.Method == http.MethodGet && p == "/v1/payment_intents":
		var data []any
		for _, id := range f.order {
			if pi := f.intents[id]; pi["customer"] == q.Get("customer") {
				data = append(data, pi)
			}
		}
		return 200, obj{"object": "list", "data": data, "has_more": false}
	case r.Method == http.MethodPost && seg[0] == "payment_intents" && len(seg) == 3 && seg[2] == "cancel":
		pi, ok := f.intents[seg[1]]
		if !ok {
			return 404, stripeErr("resource_missing")
		}
		if pi["status"] == "succeeded" {
			return 400, stripeErr("payment_intent_unexpected_state")
		}
		pi["status"] = "canceled"
		if reason := form.Get("cancellation_reason"); reason != "" {
			pi["cancellation_reason"] = reason
		}
		return 200, pi
	case r.Method == http.MethodGet && seg[0] == "payment_intents" && len(seg) == 2:
		if pi, ok := f.intents[seg[1]]; ok {
			return 200, pi
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodGet && seg[0] == "charges" && len(seg) == 2:
		if ch, ok := f.charges[seg[1]]; ok {
			return 200, ch
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodPost && p == "/v1/refunds":
		return f.createRefund(form)
	case r.Method == http.MethodGet && seg[0] == "refunds" && len(seg) == 2:
		if re, ok := f.refunds[seg[1]]; ok {
			return 200, re
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodGet && p == "/v1/refunds":
		var data []any
		for _, re := range f.refunds {
			if (q.Get("charge") == "" || re["charge"] == q.Get("charge")) && (q.Get("payment_intent") == "" || re["payment_intent"] == q.Get("payment_intent")) {
				data = append(data, re)
			}
		}
		return 200, obj{"object": "list", "data": data, "has_more": false}
	case r.Method == http.MethodGet && p == "/v1/subscriptions":
		var data []any
		for _, s := range f.subs {
			if q.Get("customer") == "" || s["customer"] == q.Get("customer") {
				data = append(data, s)
			}
		}
		return 200, obj{"object": "list", "data": data, "has_more": false}
	case seg[0] == "subscriptions" && len(seg) == 2:
		s, ok := f.subs[seg[1]]
		if !ok {
			return 404, stripeErr("resource_missing")
		}
		switch r.Method {
		case http.MethodPost:
			if v := form.Get("cancel_at_period_end"); v != "" {
				s["cancel_at_period_end"] = v == "true"
			}
		case http.MethodDelete:
			s["status"], s["canceled_at"] = "canceled", time.Now().Unix()
		}
		return 200, s
	}
	f.odd = append(f.odd, r.Method+" "+p)
	return 404, stripeErr("resource_missing")
}

func stripeErr(code string) obj {
	return obj{"error": obj{"type": "invalid_request_error", "code": code, "message": code}}
}

func (f *stripeFake) createIntent(form url.Values) (int, any) {
	amount, _ := strconv.ParseInt(form.Get("amount"), 10, 64)
	pm := form.Get("payment_method")
	pi := obj{"object": "payment_intent", "id": f.id("pi"), "amount": amount, "amount_received": 0, "currency": form.Get("currency"), "customer": form.Get("customer"),
		"payment_method": pm, "capture_method": form.Get("capture_method"), "confirmation_method": form.Get("confirmation_method"), "setup_future_usage": form.Get("setup_future_usage"),
		"livemode": false, "metadata": metadataOf(form), "created": time.Now().Unix()}
	pi["client_secret"] = pi["id"].(string) + "_secret_gf"
	f.intents[pi["id"].(string)] = pi
	f.order = append(f.order, pi["id"].(string))
	switch decline := f.declines[pm]; decline {
	case "":
		ch := obj{"object": "charge", "id": f.id("ch"), "amount": amount, "amount_captured": amount, "currency": form.Get("currency"), "customer": form.Get("customer"), "payment_method": pm,
			"payment_intent": pi["id"], "status": "succeeded", "paid": true, "captured": true, "refunded": false, "amount_refunded": int64(0), "disputed": false, "livemode": false}
		f.charges[ch["id"].(string)] = ch
		pi["status"], pi["amount_received"], pi["latest_charge"] = "succeeded", amount, ch["id"]
		return 200, pi
	case "auth":
		pi["status"] = "requires_action"
		return 200, pi
	default:
		// Stripe detaches the refused method from the intent; it survives
		// only on last_payment_error.
		pi["status"], pi["payment_method"] = "requires_payment_method", nil
		pi["last_payment_error"] = obj{"type": "card_error", "code": "card_declined", "decline_code": decline, "payment_method": obj{"id": pm, "object": "payment_method"}}
		return 402, obj{"error": obj{"type": "card_error", "code": "card_declined", "decline_code": decline, "payment_intent": pi}}
	}
}

func (f *stripeFake) createRefund(form url.Values) (int, any) {
	var ch obj
	if id := form.Get("charge"); id != "" {
		ch = f.charges[id]
	} else if pi := f.intents[form.Get("payment_intent")]; pi != nil {
		ch = f.charges[fmt.Sprint(pi["latest_charge"])]
	}
	if ch == nil {
		return 404, stripeErr("resource_missing")
	}
	amount, _ := strconv.ParseInt(form.Get("amount"), 10, 64)
	remaining := ch["amount"].(int64) - ch["amount_refunded"].(int64)
	if amount == 0 {
		amount = remaining
	}
	if amount > remaining {
		return 400, stripeErr("charge_already_refunded")
	}
	ch["amount_refunded"] = ch["amount_refunded"].(int64) + amount
	ch["refunded"] = ch["amount_refunded"] == ch["amount"]
	re := obj{"object": "refund", "id": f.id("re"), "amount": amount, "charge": ch["id"], "payment_intent": ch["payment_intent"], "currency": ch["currency"], "status": "succeeded",
		"reason": form.Get("reason"), "metadata": metadataOf(form), "created": time.Now().Unix()}
	f.refunds[re["id"].(string)] = re
	return 200, re
}

// completeSetup is the browser confirming a SetupIntent with a new card.
func (f *stripeFake) completeSetup(setupID string, c card) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.setups[setupID]
	pm := f.id("pm")
	f.methods[pm] = obj{"object": "payment_method", "id": pm, "type": "card", "customer": s["customer"], "livemode": false,
		"card": obj{"brand": c.Brand, "last4": c.Last4, "exp_month": 12, "exp_year": 2035}}
	f.declines[pm] = c.Decline
	s["status"], s["payment_method"] = "succeeded", pm
}

// setDecline changes how the issuer answers future charges on every card
// ending in last4.
func (f *stripeFake) setDecline(last4, decline string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, m := range f.methods {
		if m["card"].(obj)["last4"] == last4 {
			f.declines[id] = decline
		}
	}
}

// ledger is the provider's view of money moved for a customer: succeeded
// charges and their refunded amounts.
type ledgerEntry struct {
	ID, Method string
	Amount     int64
	Refunded   int64
}

func (f *stripeFake) ledger(stripeCustomer string) []ledgerEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ledgerEntry
	for _, id := range f.order {
		pi := f.intents[id]
		if (stripeCustomer != "" && pi["customer"] != stripeCustomer) || pi["status"] != "succeeded" {
			continue
		}
		ch := f.charges[pi["latest_charge"].(string)]
		out = append(out, ledgerEntry{ID: pi["id"].(string), Method: pi["payment_method"].(string), Amount: ch["amount"].(int64), Refunded: ch["amount_refunded"].(int64)})
	}
	return out
}

// customerOf returns the Stripe customer that owns payment method pm.
func (f *stripeFake) customerOf(pm string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprint(f.methods[pm]["customer"])
}

// cardOf is the last4 of a Stripe payment method.
func (f *stripeFake) cardOf(pm string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprint(f.methods[pm]["card"].(obj)["last4"])
}

// attempts counts PaymentIntent creations for a Stripe customer.
func (f *stripeFake) attempts(stripeCustomer string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, id := range f.order {
		if stripeCustomer == "" || f.intents[id]["customer"] == stripeCustomer {
			n++
		}
	}
	return n
}

func (f *stripeFake) mutations(path string) []providerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []providerCall
	for _, c := range f.writes {
		if strings.HasPrefix(c.Path, path) {
			out = append(out, c)
		}
	}
	return out
}

func (f *stripeFake) unexpected() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.odd...)
	sort.Strings(out)
	return out
}
