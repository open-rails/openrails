package stripemock

import (
	"bytes"
	"encoding/json"
	"errors"
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

// Object is one Stripe API object as its JSON decodes.
type Object = map[string]any

// Options configure a Mock.
type Options struct {
	// Clock is the mock's only time source. Nil means time.Now.
	Clock func() time.Time
}

// Card is a card as a browser enters it. Decline "" approves; a Stripe
// decline_code declines its charges; "auth" makes them require issuer
// authentication.
type Card struct {
	Brand, Last4, Decline string
}

// Call is one request the mock received.
type Call struct {
	Method, Path   string
	IdempotencyKey string
	Form           url.Values
}

// Stripe retains an executed request's status and body, including declines,
// for at least 24 hours. The mock chooses the earliest permitted expiry.
// https://docs.stripe.com/api/idempotent_requests
type idempotentRequest struct {
	method, path, params string
	createdAt            time.Time
	status               int
	body                 []byte
}

// Mock is a stateful Stripe. It is an http.Handler (for a listener) and an
// http.RoundTripper (for in-process clients). All methods are safe for
// concurrent use.
type Mock struct {
	server *httptest.Server

	mu              sync.Mutex
	seq             int
	now             func() time.Time
	customers       map[string]Object
	setups          map[string]Object
	methods         map[string]Object
	declines        map[string]string
	intents         map[string]Object
	order           []string
	charges         map[string]Object
	refunds         map[string]Object
	refundOrder     []string
	subs            map[string]Object
	sessions        map[string]Object
	sessionOrder    []string
	idem            map[string]*idempotentRequest
	visibilityDelay time.Duration
	visibleAt       map[string]time.Time
	requests        []Call
	writes          []Call
	intercepts      []*intercept
	odd             []string
	lose, lost      int
	listDown        bool
	chargesDown     bool
	subsDown        bool
	pricesDown      bool
	// amounts are legacy prices created at Stripe at other than 9.99.
	amounts map[string]int64
	webhook webhookTarget
	events  []Event
}

// New starts a Mock on a loopback listener; see URL and Close.
func New(opts Options) *Mock {
	m := NewUnstarted(opts)
	m.server = httptest.NewServer(m)
	return m
}

// NewUnstarted is a Mock with no listener, for use as a RoundTripper or a
// Handler the caller serves.
func NewUnstarted(opts Options) *Mock {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Mock{now: opts.Clock, customers: map[string]Object{}, setups: map[string]Object{}, methods: map[string]Object{}, declines: map[string]string{},
		intents: map[string]Object{}, charges: map[string]Object{}, refunds: map[string]Object{}, subs: map[string]Object{}, sessions: map[string]Object{},
		idem: map[string]*idempotentRequest{}, visibleAt: map[string]time.Time{}}
}

// URL is the loopback API root; OpenRails' provider_sandbox.stripe_api_url.
func (m *Mock) URL() string {
	if m.server == nil {
		panic("stripemock: URL of an unstarted mock")
	}
	return m.server.URL
}

// Close stops the listener, if any.
func (m *Mock) Close() {
	if m.server != nil {
		m.server.Close()
	}
}

func (m *Mock) id(prefix string) string {
	m.seq++
	return fmt.Sprintf("%s_gf%06d", prefix, m.seq)
}

// ServeHTTP serves the API on a listener. A lost request aborts the
// connection.
func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	res, err := m.RoundTrip(r)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	defer res.Body.Close()
	for k, v := range res.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
}

// RoundTrip serves r in process.
func (m *Mock) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	form, _ := url.ParseQuery(string(body))
	m.mu.Lock()
	if r.Method != http.MethodGet {
		m.requests = append(m.requests, Call{Method: r.Method, Path: r.URL.Path, IdempotencyKey: r.Header.Get("Idempotency-Key"), Form: form})
	}
	creates := r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents"
	lost := m.lose > 0 && creates
	if lost {
		m.lose--
		m.lost++
	}
	down := m.listDown && r.Method == http.MethodGet && r.URL.Path == "/v1/payment_intents" ||
		m.chargesDown && r.Method == http.MethodGet && r.URL.Path == "/v1/charges" ||
		m.subsDown && r.Method != http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/subscriptions/") ||
		m.pricesDown && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/prices/")
	intercepts := append([]*intercept(nil), m.intercepts...)
	m.mu.Unlock()
	var hook *intercept
	for _, ic := range intercepts {
		if ic.match(r) {
			hook = ic
		}
	}
	if lost {
		return nil, errors.New("stripemock: connection reset before Stripe received the request")
	}
	if down {
		return respond(r, http.StatusServiceUnavailable, []byte(`{"error":{"type":"api_error","message":"unavailable"}}`)), nil
	}
	replay, key, claimed := m.beginIdempotentRequest(r, form)
	if replay != nil {
		return respond(r, replay.status, replay.body), nil
	}
	defer m.abandonIdempotentRequest(key, claimed)
	serve := func() *http.Response {
		status, raw := m.serve(r, form, claimed)
		return respond(r, status, raw)
	}
	if hook != nil {
		return hook.fn(r, serve)
	}
	return serve(), nil
}

func respond(r *http.Request, status int, raw []byte) *http.Response {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(status)
	_, _ = rec.Write(raw)
	res := rec.Result()
	res.Request = r
	return res
}

type stored struct {
	status int
	body   []byte
}

func (m *Mock) beginIdempotentRequest(r *http.Request, form url.Values) (*stored, string, *idempotentRequest) {
	key := r.Header.Get("Idempotency-Key")
	if r.Method != http.MethodPost || key == "" {
		return nil, "", nil
	}
	key = r.Header.Get("Stripe-Account") + "|" + key
	m.mu.Lock()
	defer m.mu.Unlock()
	if previous := m.idem[key]; previous != nil {
		switch {
		case previous.status != 0 && !m.now().Before(previous.createdAt.Add(24*time.Hour)):
			delete(m.idem, key)
		case previous.method != r.Method || previous.path != r.URL.Path || previous.params != form.Encode():
			return &stored{http.StatusBadRequest, []byte(`{"error":{"type":"idempotency_error","message":"Keys for idempotent requests can only be used with the same parameters"}}`)}, key, nil
		case previous.status == 0:
			return &stored{http.StatusConflict, []byte(`{"error":{"type":"invalid_request_error","code":"idempotency_key_in_use","message":"Another request is executing with this idempotency key"}}`)}, key, nil
		default:
			return &stored{previous.status, previous.body}, key, nil
		}
	}
	claim := &idempotentRequest{method: r.Method, path: r.URL.Path, params: form.Encode(), createdAt: m.now()}
	m.idem[key] = claim
	return nil, key, claim
}

func (m *Mock) abandonIdempotentRequest(key string, claim *idempotentRequest) {
	if claim == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idem[key] == claim && claim.status == 0 {
		delete(m.idem, key)
	}
}

func (m *Mock) serve(r *http.Request, form url.Values, claimed *idempotentRequest) (int, []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Method != http.MethodGet {
		m.writes = append(m.writes, Call{Method: r.Method, Path: r.URL.Path, IdempotencyKey: r.Header.Get("Idempotency-Key"), Form: form})
	}
	status, out := m.route(r, form)
	raw, _ := json.Marshal(out)
	// Validation failures do not start an operation. Executed outcomes,
	// including issuer declines and server failures, retain status and body.
	if claimed != nil && status != http.StatusBadRequest && status != http.StatusNotFound {
		claimed.status, claimed.body = status, raw
	}
	return status, raw
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

func (m *Mock) route(r *http.Request, form url.Values) (int, any) {
	p, q := r.URL.Path, r.URL.Query()
	seg := strings.Split(strings.TrimPrefix(p, "/v1/"), "/")
	switch {
	case p == "/v1/account":
		// A key sk_test_acct_<name> is account acct_<name>'s; any other is acct_e2e's.
		account := "acct_e2e"
		if name, ok := strings.CutPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "sk_test_acct_"); ok {
			account = "acct_" + name
		}
		return 200, Object{"object": "account", "id": account, "charges_enabled": true}
	case p == "/v1/balance":
		return 200, Object{"object": "balance", "livemode": false, "available": []any{}, "pending": []any{}}
	case r.Method == http.MethodGet && p == "/v1/customers/search":
		data := []any{}
		for _, c := range m.customers {
			match := true
			for _, mq := range metadataQuery.FindAllStringSubmatch(q.Get("query"), -1) {
				if c["metadata"].(map[string]string)[mq[1]] != mq[2] {
					match = false
				}
			}
			if match {
				data = append(data, c)
			}
		}
		return 200, Object{"object": "search_result", "data": data, "has_more": false}
	case r.Method == http.MethodPost && p == "/v1/customers":
		c := Object{"object": "customer", "id": m.id("cus"), "email": form.Get("email"), "metadata": metadataOf(form), "livemode": false}
		m.customers[c["id"].(string)] = c
		return 200, c
	case r.Method == http.MethodGet && seg[0] == "customers" && len(seg) == 2:
		if c, ok := m.customers[seg[1]]; ok {
			return 200, c
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodPost && p == "/v1/setup_intents":
		s := Object{"object": "setup_intent", "id": m.id("seti"), "status": "requires_payment_method", "customer": form.Get("customer"), "usage": form.Get("usage"),
			"payment_method_types": []string{"card"}, "livemode": false, "metadata": metadataOf(form)}
		s["client_secret"] = s["id"].(string) + "_secret_gf"
		m.setups[s["id"].(string)] = s
		if pm := form.Get("payment_method"); pm != "" && form.Get("confirm") == "true" {
			// A server-confirmed setup of an attached card, its customer present.
			s["payment_method"] = pm
			switch decline := m.declines[pm]; decline {
			case "":
				s["status"] = "succeeded"
				if method, ok := m.methods[pm]; ok && method["customer"] == nil {
					method["customer"] = form.Get("customer")
				}
			case "auth":
				s["status"] = "requires_action"
			default:
				s["status"], s["payment_method"] = "requires_payment_method", nil
				return 402, Object{"error": Object{"type": "card_error", "code": "card_declined", "decline_code": decline, "setup_intent": s}}
			}
		}
		return 200, s
	case r.Method == http.MethodPost && seg[0] == "setup_intents" && len(seg) == 3 && seg[2] == "cancel":
		s, ok := m.setups[seg[1]]
		if !ok {
			return 404, stripeErr("resource_missing")
		}
		s["status"] = "canceled"
		return 200, s
	case r.Method == http.MethodPost && seg[0] == "payment_methods" && len(seg) == 2:
		pm, ok := m.methods[seg[1]]
		if !ok {
			return 404, stripeErr("resource_missing")
		}
		c := pm["card"].(Object)
		for _, field := range []string{"exp_month", "exp_year"} {
			if v := form.Get("card[" + field + "]"); v != "" {
				n, _ := strconv.Atoi(v)
				c[field] = n
			}
		}
		details, _ := pm["billing_details"].(Object)
		if details == nil {
			details = Object{}
		}
		for key, values := range form {
			if field, ok := strings.CutPrefix(key, "billing_details["); ok {
				details[strings.TrimSuffix(field, "]")] = values[0]
			}
		}
		pm["billing_details"] = details
		return 200, pm
	case r.Method == http.MethodGet && seg[0] == "setup_intents" && len(seg) == 2:
		if s, ok := m.setups[seg[1]]; ok {
			return 200, s
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodGet && seg[0] == "prices" && len(seg) == 2 && strings.HasPrefix(seg[1], "price_legacy_"):
		// A legacy monthly price: 9.99 unless SetLegacyPrice set it.
		amount, ok := m.amounts[seg[1]]
		if !ok {
			amount = 999
		}
		return 200, Object{"object": "price", "id": seg[1], "product": "prod_legacy", "unit_amount": amount, "currency": "usd", "active": true, "livemode": false,
			"type": "recurring", "recurring": Object{"interval": "month", "interval_count": 1}, "metadata": map[string]string{}}
	case r.Method == http.MethodGet && seg[0] == "payment_methods" && len(seg) == 2:
		if pm, ok := m.methods[seg[1]]; ok {
			return 200, pm
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodPost && p == "/v1/payment_intents":
		return m.createIntent(form)
	case r.Method == http.MethodGet && p == "/v1/payment_intents":
		data := []any{}
		for _, id := range m.order {
			if pi := m.intents[id]; pi["customer"] == q.Get("customer") && !m.now().Before(m.visibleAt[id]) {
				data = append(data, pi)
			}
		}
		return 200, Object{"object": "list", "data": data, "has_more": false}
	case r.Method == http.MethodPost && seg[0] == "payment_intents" && len(seg) == 3 && seg[2] == "cancel":
		pi, ok := m.intents[seg[1]]
		if !ok {
			return 404, stripeErr("resource_missing")
		}
		if pi["status"] == "succeeded" {
			return 400, stripeErr("payment_intent_unexpected_state")
		}
		// Stripe clears the card and the decline when it cancels an intent.
		pi["status"], pi["payment_method"] = "canceled", nil
		delete(pi, "last_payment_error")
		if reason := form.Get("cancellation_reason"); reason != "" {
			pi["cancellation_reason"] = reason
		}
		return 200, pi
	case r.Method == http.MethodGet && seg[0] == "payment_intents" && len(seg) == 2:
		if pi, ok := m.intents[seg[1]]; ok {
			return 200, pi
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodGet && seg[0] == "charges" && len(seg) == 2:
		if ch, ok := m.charges[seg[1]]; ok {
			return 200, ch
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodGet && p == "/v1/charges":
		return 200, stripeList(m.charges, q)
	case r.Method == http.MethodGet && p == "/v1/disputes":
		return 200, Object{"object": "list", "data": []any{}, "has_more": false}
	case r.Method == http.MethodPost && p == "/v1/refunds":
		return m.createRefund(form)
	case r.Method == http.MethodGet && seg[0] == "refunds" && len(seg) == 2:
		if re, ok := m.refunds[seg[1]]; ok {
			return 200, re
		}
		return 404, stripeErr("resource_missing")
	case r.Method == http.MethodGet && p == "/v1/refunds":
		matching := map[string]Object{}
		for _, re := range m.refunds {
			if (q.Get("charge") == "" || re["charge"] == q.Get("charge")) && (q.Get("payment_intent") == "" || re["payment_intent"] == q.Get("payment_intent")) {
				matching[re["id"].(string)] = re
			}
		}
		return 200, stripeList(matching, q)
	case r.Method == http.MethodGet && p == "/v1/subscriptions":
		data := []any{}
		for _, s := range m.subs {
			if q.Get("customer") == "" || s["customer"] == q.Get("customer") {
				data = append(data, s)
			}
		}
		return 200, Object{"object": "list", "data": data, "has_more": false}
	case seg[0] == "subscriptions" && len(seg) == 2:
		s, ok := m.subs[seg[1]]
		if !ok {
			return 404, stripeErr("resource_missing")
		}
		switch r.Method {
		case http.MethodPost:
			if v := form.Get("cancel_at_period_end"); v != "" {
				s["cancel_at_period_end"] = v == "true"
			}
		case http.MethodDelete:
			s["status"], s["canceled_at"] = "canceled", m.now().Unix()
		}
		return 200, s
	case r.Method == http.MethodPost && p == "/v1/billing_portal/sessions":
		if _, ok := m.customers[form.Get("customer")]; !ok {
			return 400, stripeErr("resource_missing")
		}
		id := m.id("bps")
		return 200, Object{"object": "billing_portal.session", "id": id, "customer": form.Get("customer"), "return_url": form.Get("return_url"),
			"url": "https://billing.stripe.com/p/session/test_" + id, "livemode": false}
	case r.Method == http.MethodPost && p == "/v1/checkout/sessions":
		return m.createCheckoutSession(form)
	case seg[0] == "checkout" && len(seg) >= 3 && seg[1] == "sessions":
		s, ok := m.sessions[seg[2]]
		switch {
		case !ok:
			return 404, stripeErr("resource_missing")
		case r.Method == http.MethodGet && len(seg) == 3:
			return 200, s
		case r.Method == http.MethodPost && len(seg) == 4 && seg[3] == "expire":
			if s["status"] != "open" {
				return 400, stripeErr("checkout_session_not_open")
			}
			s["status"] = "expired"
			return 200, s
		}
	}
	m.odd = append(m.odd, r.Method+" "+p)
	return 404, stripeErr("resource_missing")
}

func stripeErr(code string) Object {
	return Object{"error": Object{"type": "invalid_request_error", "code": code, "message": code}}
}

// stripeList implements the created window and stable cursor used by provider
// refresh. Returning an unfiltered page would hide missing recovery windows.
func stripeList(objects map[string]Object, q url.Values) Object {
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
			return Object{"object": "list", "data": data, "has_more": true}
		}
		data = append(data, object)
	}
	return Object{"object": "list", "data": data, "has_more": false}
}

func (m *Mock) createIntent(form url.Values) (int, any) {
	amount, err := strconv.ParseInt(form.Get("amount"), 10, 64)
	if err != nil || amount <= 0 {
		return http.StatusBadRequest, stripeErr("parameter_invalid_integer")
	}
	pm := form.Get("payment_method")
	if form.Get("currency") == "" {
		return http.StatusBadRequest, stripeErr("parameter_missing")
	}
	pi := Object{"object": "payment_intent", "id": m.id("pi"), "amount": amount, "amount_received": 0, "currency": form.Get("currency"), "customer": form.Get("customer"),
		"payment_method": pm, "capture_method": form.Get("capture_method"), "confirmation_method": form.Get("confirmation_method"), "setup_future_usage": form.Get("setup_future_usage"),
		"livemode": false, "metadata": metadataOf(form), "created": m.now().Unix()}
	pi["client_secret"] = pi["id"].(string) + "_secret_gf"
	m.intents[pi["id"].(string)] = pi
	m.visibleAt[pi["id"].(string)] = m.now().Add(m.visibilityDelay)
	m.order = append(m.order, pi["id"].(string))
	switch decline := m.declines[pm]; decline {
	case "":
		m.chargeLocked(pi)["created"] = m.now().Unix()
		return 200, pi
	case "auth":
		pi["status"] = "requires_action"
		return 200, pi
	default:
		// Stripe detaches the refused method from the intent; it survives
		// only on last_payment_error.
		pi["status"], pi["payment_method"] = "requires_payment_method", nil
		pi["last_payment_error"] = Object{"type": "card_error", "code": "card_declined", "decline_code": decline, "payment_method": Object{"id": pm, "object": "payment_method"}}
		return 402, Object{"error": Object{"type": "card_error", "code": "card_declined", "decline_code": decline, "payment_intent": pi}}
	}
}

// chargeLocked settles a payment intent with one succeeded charge; one that
// saves its card attaches it to the intent's customer.
func (m *Mock) chargeLocked(pi Object) Object {
	if pm, ok := m.methods[fmt.Sprint(pi["payment_method"])]; ok && pi["setup_future_usage"] != nil && pi["setup_future_usage"] != "" && pm["customer"] == nil {
		pm["customer"] = pi["customer"]
	}
	amount := pi["amount"].(int64)
	ch := Object{"object": "charge", "id": m.id("ch"), "amount": amount, "amount_captured": amount, "currency": pi["currency"], "customer": pi["customer"], "payment_method": pi["payment_method"],
		"payment_intent": pi["id"], "status": "succeeded", "paid": true, "captured": true, "refunded": false, "amount_refunded": int64(0), "disputed": false, "livemode": false}
	m.charges[ch["id"].(string)] = ch
	pi["status"], pi["amount_received"], pi["latest_charge"] = "succeeded", amount, ch["id"]
	return ch
}

func (m *Mock) createRefund(form url.Values) (int, any) {
	var ch Object
	if id := form.Get("charge"); id != "" {
		ch = m.charges[id]
	} else if pi := m.intents[form.Get("payment_intent")]; pi != nil {
		ch = m.charges[fmt.Sprint(pi["latest_charge"])]
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
	re := Object{"object": "refund", "id": m.id("re"), "amount": amount, "charge": ch["id"], "payment_intent": ch["payment_intent"], "currency": ch["currency"], "status": "succeeded",
		"reason": form.Get("reason"), "metadata": metadataOf(form), "created": m.now().Unix()}
	m.refunds[re["id"].(string)] = re
	m.refundOrder = append(m.refundOrder, re["id"].(string))
	return 200, re
}

// clone is a deep copy of o through JSON, so callers never alias live state.
func clone(o Object) Object {
	raw, _ := json.Marshal(o)
	out := Object{}
	_ = json.Unmarshal(raw, &out)
	return out
}
