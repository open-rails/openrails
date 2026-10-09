//go:build e2e && integration

package ci_test

// A trimmed copy of ci/subscriptions' Stripe fake: customers, setup intents,
// payment methods and payment intents with charges, idempotent replay and a
// journal of requests it does not model.

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

	"github.com/open-rails/openrails/internal/nmimock"
)

// card is what a browser tokenizes. Decline is "" (approve), a Stripe
// decline_code, or "auth" (issuer authentication).
type card = nmimock.Card

var visa = card{Brand: "visa", Last4: "4242"}

const (
	whsecStripe = "whsec_e2e"
	monthHours  = 720
)

type obj = map[string]any

// Stripe retains an executed request's status and body for at least 24 hours.
type stripeIdempotentRequest struct {
	method, path, params string
	status               int
	body                 []byte
}

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
	idem      map[string]*stripeIdempotentRequest
	odd       []string
}

func newStripeFake() *stripeFake {
	return &stripeFake{customers: map[string]obj{}, setups: map[string]obj{}, methods: map[string]obj{}, declines: map[string]string{},
		intents: map[string]obj{}, charges: map[string]obj{}, idem: map[string]*stripeIdempotentRequest{}}
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
	replay, key, claimed := f.beginIdempotentRequest(r, form)
	if replay != nil {
		res := replay.Result()
		res.Request = r
		return res, nil
	}
	defer f.abandonIdempotentRequest(key, claimed)
	res := f.serve(r, form, claimed).Result()
	res.Request = r
	return res, nil
}

func stripeResponse(status int, raw []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(status)
	_, _ = rec.Write(raw)
	return rec
}

func (f *stripeFake) beginIdempotentRequest(r *http.Request, form url.Values) (*httptest.ResponseRecorder, string, *stripeIdempotentRequest) {
	key := r.Header.Get("Idempotency-Key")
	if r.Method != http.MethodPost || key == "" {
		return nil, "", nil
	}
	key = r.Header.Get("Stripe-Account") + "|" + key
	f.mu.Lock()
	defer f.mu.Unlock()
	if previous := f.idem[key]; previous != nil {
		switch {
		case previous.method != r.Method || previous.path != r.URL.Path || previous.params != form.Encode():
			return stripeResponse(http.StatusBadRequest, []byte(`{"error":{"type":"idempotency_error","message":"Keys for idempotent requests can only be used with the same parameters"}}`)), key, nil
		case previous.status == 0:
			return stripeResponse(http.StatusConflict, []byte(`{"error":{"type":"invalid_request_error","code":"idempotency_key_in_use","message":"Another request is executing with this idempotency key"}}`)), key, nil
		default:
			return stripeResponse(previous.status, previous.body), key, nil
		}
	}
	claim := &stripeIdempotentRequest{method: r.Method, path: r.URL.Path, params: form.Encode()}
	f.idem[key] = claim
	return nil, key, claim
}

func (f *stripeFake) abandonIdempotentRequest(key string, claim *stripeIdempotentRequest) {
	if claim == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idem[key] == claim && claim.status == 0 {
		delete(f.idem, key)
	}
}

func (f *stripeFake) serve(r *http.Request, form url.Values, claimed *stripeIdempotentRequest) *httptest.ResponseRecorder {
	f.mu.Lock()
	defer f.mu.Unlock()
	status, out := f.route(r, form)
	raw, _ := json.Marshal(out)
	// Validation failures do not start an operation; executed outcomes keep
	// their status and body.
	if claimed != nil && status != http.StatusBadRequest && status != http.StatusNotFound {
		claimed.status, claimed.body = status, raw
	}
	return stripeResponse(status, raw)
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
		// A key sk_test_acct_<name> is account acct_<name>'s; any other is acct_e2e's.
		account := "acct_e2e"
		if name, ok := strings.CutPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "sk_test_acct_"); ok {
			account = "acct_" + name
		}
		return 200, obj{"object": "account", "id": account, "charges_enabled": true}
	case p == "/v1/balance":
		return 200, obj{"object": "balance", "livemode": false, "available": []any{}, "pending": []any{}}
	case r.Method == http.MethodGet && p == "/v1/customers/search":
		data := []any{}
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
		data := []any{}
		for _, id := range f.order {
			if pi := f.intents[id]; pi["customer"] == q.Get("customer") {
				data = append(data, pi)
			}
		}
		return 200, obj{"object": "list", "data": data, "has_more": false}
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
	case r.Method == http.MethodGet && p == "/v1/charges":
		return 200, stripeList(f.charges, q)
	case r.Method == http.MethodGet && (p == "/v1/disputes" || p == "/v1/refunds" || p == "/v1/subscriptions"):
		return 200, obj{"object": "list", "data": []any{}, "has_more": false}
	}
	f.odd = append(f.odd, r.Method+" "+p)
	return 404, stripeErr("resource_missing")
}

func stripeErr(code string) obj {
	return obj{"error": obj{"type": "invalid_request_error", "code": code, "message": code}}
}

// stripeList implements the created window and stable cursor used by provider
// refresh.
func stripeList(objects map[string]obj, q url.Values) obj {
	ids := make([]string, 0, len(objects))
	for id := range objects {
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	after := q.Get("starting_after")
	started := after == ""
	data := []any{}
	for _, id := range ids {
		if !started {
			started = id == after
			continue
		}
		object := objects[id]
		created, _ := object["created"].(int64)
		if lower, err := strconv.ParseInt(q.Get("created[gte]"), 10, 64); err == nil && created < lower {
			continue
		}
		if upper, err := strconv.ParseInt(q.Get("created[lte]"), 10, 64); err == nil && created > upper {
			continue
		}
		if q.Get("customer") != "" && object["customer"] != q.Get("customer") {
			continue
		}
		if len(data) == limit {
			return obj{"object": "list", "data": data, "has_more": true}
		}
		data = append(data, object)
	}
	return obj{"object": "list", "data": data, "has_more": false}
}

func (f *stripeFake) createIntent(form url.Values) (int, any) {
	amount, err := strconv.ParseInt(form.Get("amount"), 10, 64)
	if err != nil || amount <= 0 {
		return http.StatusBadRequest, stripeErr("parameter_invalid_integer")
	}
	pm := form.Get("payment_method")
	if form.Get("currency") == "" {
		return http.StatusBadRequest, stripeErr("parameter_missing")
	}
	now := time.Now().Unix()
	pi := obj{"object": "payment_intent", "id": f.id("pi"), "amount": amount, "amount_received": 0, "currency": form.Get("currency"), "customer": form.Get("customer"),
		"payment_method": pm, "capture_method": form.Get("capture_method"), "confirmation_method": form.Get("confirmation_method"), "setup_future_usage": form.Get("setup_future_usage"),
		"livemode": false, "metadata": metadataOf(form), "created": now}
	pi["client_secret"] = pi["id"].(string) + "_secret_gf"
	f.intents[pi["id"].(string)] = pi
	f.order = append(f.order, pi["id"].(string))
	switch decline := f.declines[pm]; decline {
	case "":
		ch := obj{"object": "charge", "id": f.id("ch"), "amount": amount, "amount_captured": amount, "currency": form.Get("currency"), "customer": form.Get("customer"), "payment_method": pm,
			"payment_intent": pi["id"], "status": "succeeded", "paid": true, "captured": true, "refunded": false, "amount_refunded": int64(0), "disputed": false, "livemode": false, "created": now}
		f.charges[ch["id"].(string)] = ch
		pi["status"], pi["amount_received"], pi["latest_charge"] = "succeeded", amount, ch["id"]
		return 200, pi
	case "auth":
		pi["status"] = "requires_action"
		return 200, pi
	default:
		pi["status"], pi["payment_method"] = "requires_payment_method", nil
		pi["last_payment_error"] = obj{"type": "card_error", "code": "card_declined", "decline_code": decline, "payment_method": obj{"id": pm, "object": "payment_method"}}
		return 402, obj{"error": obj{"type": "card_error", "code": "card_declined", "decline_code": decline, "payment_intent": pi}}
	}
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

// charged counts the succeeded payment intents: the money Stripe moved.
func (f *stripeFake) charged() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, id := range f.order {
		if f.intents[id]["status"] == "succeeded" {
			n++
		}
	}
	return n
}

func (f *stripeFake) unexpected() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.odd...)
	sort.Strings(out)
	return out
}
