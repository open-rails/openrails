//go:build greenfield && integration

package subscriptions_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type nmiVault struct {
	ID, BillingID string
	Card          card
	// Extra are further billing entries (cards) in the same vault, after the
	// priority-1 entry.
	Extra []nmiBilling
	// NoBrand: NMI's customer record omits card_type, as live accounts do.
	NoBrand bool
}

type nmiBilling struct {
	ID   string
	Card card
}

type nmiSale struct {
	TransactionID, OrderID, Vault, BillingID, Amount, Currency string
	InitiatedBy, Indicator, Initial                            string
	Card                                                       card
	RefundedCents                                              int64
	RefundIDs                                                  []string
	ScheduleID                                                 string
	Declined                                                   string
	At                                                         time.Time
	// Hidden is a processed sale the Query API does not show yet.
	Hidden bool
}

// nmiSchedule is a provider-owned recurring plan subscription. Days or
// Months is its plan cadence (Days defaults to 30). A deleted schedule stays
// readable as NMI's tombstone (delayed_condition=inactive) and leaves the list.
type nmiSchedule struct {
	ID, Vault, Plan, Amount string
	Order, Days, Months     string
	NextBilling             time.Time
	Deleted, Paused         bool
	// Custom is a custom-amount schedule (NMI shows its plan with an empty
	// plan_name); otherwise the schedule is on the named plan Plan.
	Custom bool
}

// nmiFake is a stateful NMI gateway: Customer Vault (v5), Direct Post sales,
// Query API searches, v5 transaction reads/refunds and v5 recurring
// subscriptions. Declines follow each vault's current card.
type nmiFake struct {
	mu        sync.Mutex
	seq       int
	tokens    map[string]card
	vaults    map[string]*nmiVault
	sales     []*nmiSale
	attempts  []url.Values
	schedules map[string]*nmiSchedule
	plans     map[string]obj
	duplicate int
	// dupWindow models NMI's duplicate check: a sale or verification with the
	// card and amount of one processed within the window is refused
	// unprocessed, whatever its order id. Off when zero.
	dupWindow   time.Duration
	dupNow      func() time.Time
	recent      []nmiRecent
	declined    []*nmiSale
	lose        int
	lost        int
	drop        int
	queryDown   bool
	writes      []providerCall
	validations []nmiValidation
	gates       []*gate
	odd         []string

	// refusedSaves counts vault creations refused for a card declined "vault".
	refusedSaves int
	// failUpdates makes the next n update_subscription requests fail.
	failUpdates int
	// hide makes the next n approved sales invisible to searches until reveal.
	hide int
	// clock stamps transactions; the world's engine clock.
	clock func() time.Time
}

func newNMIFake() *nmiFake {
	return &nmiFake{tokens: map[string]card{}, vaults: map[string]*nmiVault{}, schedules: map[string]*nmiSchedule{}, plans: map[string]obj{}, clock: time.Now}
}

func (f *nmiFake) now() time.Time { return f.clock().UTC() }

func (f *nmiFake) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s%08d", prefix, f.seq)
}

// tokenize is Collect.js: a single-use token for card.
func (f *nmiFake) tokenize(c card) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	token := f.next("tok-")
	f.tokens[token] = c
	return token
}

func (f *nmiFake) hold(g *gate) *gate {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates = append(f.gates, g)
	return g
}

func (f *nmiFake) unhold() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates = nil
}

func (f *nmiFake) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	var g *gate
	for _, candidate := range f.gates {
		if candidate.match(r) {
			g = candidate
		}
	}
	sale := strings.HasSuffix(r.URL.Path, "/transact.php") && !strings.Contains(string(body), "type=validate")
	lost, dropped := f.lose > 0 && sale, f.drop > 0 && sale
	if lost {
		f.lose--
		f.lost++
	} else if dropped {
		f.drop--
	}
	f.mu.Unlock()
	if lost {
		return nil, errors.New("connection reset before the gateway received the request")
	}
	if dropped {
		f.serve(r, body)
		return nil, errors.New("connection reset before the gateway's answer arrived")
	}
	if g != nil && g.served {
		rec := f.serve(r, body)
		g.once.Do(func() { close(g.arrived) })
		select {
		case <-g.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		res := rec.Result()
		res.Request = r
		return res, nil
	}
	if g != nil {
		g.once.Do(func() { close(g.arrived) })
		select {
		case <-g.release:
		case <-r.Context().Done():
			if g.commit {
				f.serve(r, body)
			}
			return nil, r.Context().Err()
		}
	}
	res := f.serve(r, body).Result()
	res.Request = r
	return res, nil
}

func (f *nmiFake) serve(r *http.Request, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	if i := strings.Index(p, "/v5/"); i >= 0 {
		rec.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			f.writes = append(f.writes, providerCall{Method: r.Method, Path: p[i+3:]})
		}
		status, out := f.v5(r.Method, strings.Split(strings.TrimPrefix(p[i+4:], "/"), "/"), body)
		raw, _ := json.Marshal(out)
		rec.WriteHeader(status)
		_, _ = rec.Write(raw)
		return rec
	}
	form, _ := url.ParseQuery(string(body))
	for k, v := range r.URL.Query() {
		form[k] = v
	}
	switch {
	case strings.HasSuffix(p, "/transact.php") && form.Get("type") == "validate":
		f.writes = append(f.writes, providerCall{Method: r.Method, Path: "transact.php", Form: form})
		_, _ = rec.WriteString(f.validate(form))
	case strings.HasSuffix(p, "/transact.php") && form.Get("type") == "sale":
		f.writes = append(f.writes, providerCall{Method: r.Method, Path: "transact.php", Form: form})
		_, _ = rec.WriteString(f.sale(form))
	case strings.HasSuffix(p, "/transact.php") && form.Get("recurring") == "update_subscription":
		f.writes = append(f.writes, providerCall{Method: r.Method, Path: "transact.php", Form: form})
		_, _ = rec.WriteString(f.updateSubscription(form))
	case strings.HasSuffix(p, "/transact.php") && form.Get("recurring") == "add_subscription":
		f.writes = append(f.writes, providerCall{Method: r.Method, Path: "transact.php", Form: form})
		_, _ = rec.WriteString(f.addSubscription(form))
	case strings.HasSuffix(p, "/query.php") && form.Get("report_type") == "recurring" && f.schedules[form.Get("subscription_id")] != nil:
		s := f.schedules[form.Get("subscription_id")]
		rec.Header().Set("Content-Type", "text/xml")
		_, _ = fmt.Fprintf(rec, "<nm_response><subscription><subscription_id>%s</subscription_id><orderid>%s</orderid><ponumber>%s</ponumber><next_charge_date>%s</next_charge_date><plan><plan_id>%s</plan_id></plan></subscription></nm_response>",
			s.ID, s.Order, s.Order, s.NextBilling.Format("2006-01-02"), s.Plan)
	case strings.HasSuffix(p, "/query.php") && form.Get("report_type") == "transaction" && f.queryDown:
		rec.WriteHeader(http.StatusServiceUnavailable)
	case strings.HasSuffix(p, "/query.php") && form.Get("report_type") == "transaction":
		rec.Header().Set("Content-Type", "text/xml")
		_, _ = rec.WriteString(f.search(form.Get("order_id"), form.Get("transaction_id"), form.Get("subscription_id"), form.Get("customer_vault_id")))
	case strings.HasSuffix(p, "/query.php") && form.Get("report_type") == "test_mode_status":
		rec.Header().Set("Content-Type", "text/xml")
		_, _ = rec.WriteString("<nm_response><test_mode_status>enabled</test_mode_status></nm_response>")
	default:
		f.odd = append(f.odd, r.Method+" "+p+" "+form.Get("report_type")+form.Get("type"))
		rec.WriteHeader(http.StatusNotFound)
	}
	return rec
}

func nmiNotFound() (int, any) {
	return 404, obj{"type": "notFound", "error_code": "E_NOT_FOUND", "message": "not found"}
}

func (f *nmiFake) customer(v *nmiVault) obj {
	details := func(c card) obj {
		prefix := map[string]string{"mastercard": "5", "amex": "3"}[c.Brand]
		if prefix == "" {
			prefix = "4"
		}
		d := obj{"card_number": prefix + "xxxxxxxxxxx" + c.Last4, "card_exp": "1235", "card_type": c.Brand}
		if v.NoBrand {
			delete(d, "card_type")
		}
		return d
	}
	billing := []obj{{"id": v.BillingID, "priority": 1, "payment_details": details(v.Card)}}
	for i, b := range v.Extra {
		billing = append(billing, obj{"id": b.ID, "priority": i + 2, "payment_details": details(b.Card)})
	}
	return obj{"object": "customer", "id": v.ID, "created": "2026-01-01T00:00:00Z", "billing": billing}
}

// cardFor is the vault card a request's billing_id names ("" = the primary).
func (v *nmiVault) cardFor(billingID string) (*card, bool) {
	if billingID == "" || billingID == v.BillingID {
		return &v.Card, true
	}
	for i := range v.Extra {
		if v.Extra[i].ID == billingID {
			return &v.Extra[i].Card, true
		}
	}
	return nil, false
}

// removeBilling deletes one billing entry; NMI promotes the next entry when
// the primary goes and refuses to empty a vault.
func (v *nmiVault) removeBilling(billingID string) bool {
	if billingID == v.BillingID {
		if len(v.Extra) == 0 {
			return false
		}
		v.BillingID, v.Card, v.Extra = v.Extra[0].ID, v.Extra[0].Card, v.Extra[1:]
		return true
	}
	for i := range v.Extra {
		if v.Extra[i].ID == billingID {
			v.Extra = append(v.Extra[:i], v.Extra[i+1:]...)
			return true
		}
	}
	return false
}

func (f *nmiFake) v5(method string, seg []string, body []byte) (int, any) {
	var in struct {
		Billing json.RawMessage `json:"billing"`
		Amount  json.RawMessage `json:"amount"`
	}
	_ = json.Unmarshal(body, &in)
	token := func() (card, bool) {
		var one struct {
			PaymentDetails struct {
				PaymentToken string `json:"payment_token"`
			} `json:"payment_details"`
		}
		var many []struct {
			PaymentDetails struct {
				PaymentToken string `json:"payment_token"`
			} `json:"payment_details"`
		}
		t := ""
		if json.Unmarshal(in.Billing, &one) == nil {
			t = one.PaymentDetails.PaymentToken
		} else if json.Unmarshal(in.Billing, &many) == nil && len(many) > 0 {
			t = many[0].PaymentDetails.PaymentToken
		}
		c, ok := f.tokens[t]
		return c, ok
	}
	switch {
	case seg[0] == "customers" && len(seg) == 1 && method == http.MethodPost:
		c, ok := token()
		if !ok {
			return 400, obj{"type": "invalid", "message": "bad token"}
		}
		if c.Decline == "vault" {
			f.refusedSaves++
			return 400, obj{"type": "invalid", "message": "card refused"}
		}
		v := &nmiVault{ID: f.next("vault"), BillingID: f.next("bill"), Card: c}
		f.vaults[v.ID] = v
		return 200, f.customer(v)
	case seg[0] == "customers" && len(seg) == 1 && method == http.MethodGet:
		data := []obj{}
		for _, id := range sortedKeys(f.vaults) {
			data = append(data, f.customer(f.vaults[id]))
		}
		return 200, obj{"customers": data, "has_more": false}
	case seg[0] == "customers" && len(seg) == 3 && seg[2] == "billing" && method == http.MethodPost:
		v, ok := f.vaults[seg[1]]
		if !ok {
			return nmiNotFound()
		}
		var one struct {
			PaymentDetails struct {
				PaymentToken string `json:"payment_token"`
			} `json:"payment_details"`
		}
		_ = json.Unmarshal(body, &one)
		c, ok := f.tokens[one.PaymentDetails.PaymentToken]
		if !ok {
			return 400, obj{"type": "invalid", "message": "bad token"}
		}
		delete(f.tokens, one.PaymentDetails.PaymentToken) // single use
		b := nmiBilling{ID: f.next("bill"), Card: c}
		v.Extra = append(v.Extra, b)
		return 200, obj{"object": "billing", "id": b.ID}
	case seg[0] == "customers" && len(seg) == 4 && seg[2] == "billing" && method == http.MethodDelete:
		v, ok := f.vaults[seg[1]]
		if !ok || !v.removeBilling(seg[3]) {
			return 400, obj{"type": "invalid", "message": "billing entry cannot be removed"}
		}
		return 200, f.customer(v)
	case seg[0] == "customers" && len(seg) == 2:
		v, ok := f.vaults[seg[1]]
		if !ok {
			return nmiNotFound()
		}
		if method == http.MethodPatch {
			if c, ok := token(); ok {
				v.Card = c
			}
		}
		if method == http.MethodDelete {
			delete(f.vaults, v.ID)
		}
		return 200, f.customer(v)
	case seg[0] == "payments" && len(seg) == 1+1 && seg[1] == "auth":
		return 200, obj{"object": "transaction", "id": f.next("probe"), "response": "1", "response_code": "100", "response_text": "SUCCESS"}
	case seg[0] == "payments" && len(seg) == 3 && seg[2] == "void":
		return 200, obj{"object": "transaction", "id": seg[1], "response": "1", "response_code": "100"}
	case seg[0] == "payments" && len(seg) == 3 && seg[2] == "refund":
		s := f.saleByID(seg[1])
		if s == nil {
			return nmiNotFound()
		}
		cents := centsOf(s.Amount) - s.RefundedCents
		if len(in.Amount) > 0 {
			var amount json.Number
			_ = json.Unmarshal(in.Amount, &amount)
			cents = centsOf(amount.String())
		}
		s.RefundedCents += cents
		refundID := f.next("rf")
		s.RefundIDs = append(s.RefundIDs, refundID)
		return 200, obj{"object": "transaction", "id": refundID, "response": "1", "response_code": "100", "response_text": "SUCCESS", "amount": decimalCents(cents)}
	case seg[0] == "payments" && len(seg) == 2 && method == http.MethodGet:
		s := f.saleByID(seg[1])
		if s == nil {
			return nmiNotFound()
		}
		response, code := "1", "100"
		if s.Declined != "" {
			response, code = "2", s.Declined
		}
		actions := []obj{{"id": s.TransactionID, "type": "sale", "amount": s.Amount, "success": s.Declined == "", "response": response, "response_code": code}}
		if s.RefundedCents > 0 {
			actions = append(actions, obj{"id": s.TransactionID + "-r", "type": "refund", "amount": decimalCents(s.RefundedCents), "success": true})
		}
		return 200, obj{"object": "transaction", "id": s.TransactionID, "response": response, "response_code": code, "amount": s.Amount, "currency": s.Currency, "customer_vault_id": s.Vault, "actions": actions}
	case seg[0] == "plans" && len(seg) == 1 && method == http.MethodPost:
		var plan struct {
			ID           string          `json:"id"`
			Name         string          `json:"plan_name"`
			Amount       json.RawMessage `json:"plan_amount"`
			DayFrequency int             `json:"day_frequency"`
		}
		_ = json.Unmarshal(body, &plan)
		f.plans[plan.ID] = obj{"object": "plan", "id": plan.ID, "plan_name": plan.Name, "plan_amount": strings.Trim(string(plan.Amount), `"`), "plan_payments": "0", "day_frequency": strconv.Itoa(plan.DayFrequency)}
		return 200, f.plans[plan.ID]
	case seg[0] == "plans" && len(seg) == 2 && strings.HasPrefix(seg[1], "gf_plan_"):
		if plan := f.plans[seg[1]]; plan != nil {
			return 200, plan
		}
		return nmiNotFound()
	case seg[0] == "plans" && len(seg) == 2 && f.plans[seg[1]] != nil:
		return 200, f.plans[seg[1]]
	case seg[0] == "plans" && len(seg) == 2 && strings.HasPrefix(seg[1], "legacy_plan_"):
		// A legacy book's monthly 9.99 plan, created at NMI long ago.
		return 200, obj{"object": "plan", "id": seg[1], "plan_name": "Legacy", "plan_amount": "9.99", "plan_payments": "0", "day_frequency": "30"}
	case seg[0] == "subscriptions" && len(seg) == 1 && method == http.MethodGet:
		data := []obj{}
		for _, id := range sortedKeys(f.schedules) {
			if s := f.schedules[id]; !s.Deleted {
				data = append(data, f.schedule(s))
			}
		}
		return 200, obj{"subscriptions": data, "has_more": false}
	case seg[0] == "subscriptions" && len(seg) == 2:
		s, ok := f.schedules[seg[1]]
		if !ok {
			return nmiNotFound()
		}
		if method == http.MethodDelete {
			s.Deleted = true
		}
		return 200, f.schedule(s)
	}
	f.odd = append(f.odd, method+" v5/"+strings.Join(seg, "/"))
	return nmiNotFound()
}

func (f *nmiFake) schedule(s *nmiSchedule) obj {
	days, months := s.Days, s.Months
	if days == "" && months == "" {
		days = "30"
	}
	condition, paused := "active", "0"
	if s.Deleted {
		condition = "inactive"
	}
	if s.Paused {
		paused = "1"
	}
	name := ""
	if !s.Custom {
		name = "Legacy " + s.Plan
		if plan := f.plans[s.Plan]; plan != nil && fmt.Sprint(plan["plan_name"]) != "" {
			name = fmt.Sprint(plan["plan_name"])
		}
	}
	return obj{"object": "subscription", "id": s.ID, "customer_vault_id": s.Vault, "delayed_condition": condition, "paused_subscription": paused, "amount": s.Amount,
		"next_billing_date": s.NextBilling.Format("2006-01-02"), "plan": obj{"id": s.Plan, "plan_name": name, "plan_amount": s.Amount, "day_frequency": days, "month_frequency": months, "plan_payments": "0"}}
}

func decimalCents(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func centsOf(amount string) int64 {
	whole, fraction, _ := strings.Cut(amount, ".")
	if len(fraction) > 2 {
		panic("fake NMI amount has fractional cents: " + amount)
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units < 0 {
		panic("invalid fake NMI amount: " + amount)
	}
	minor, err := strconv.ParseInt(fraction+strings.Repeat("0", 2-len(fraction)), 10, 64)
	if err != nil {
		panic("invalid fake NMI cents: " + amount)
	}
	return units*100 + minor
}

func (f *nmiFake) saleByID(id string) *nmiSale {
	for _, s := range f.sales {
		if s.TransactionID == id {
			return s
		}
	}
	return nil
}

func (f *nmiFake) sale(form url.Values) string {
	f.attempts = append(f.attempts, form)
	v := f.vaults[form.Get("customer_vault_id")]
	if v == nil {
		return "response=3&responsetext=Invalid+Customer+Vault+Id&response_code=300"
	}
	charged, ok := v.cardFor(form.Get("billing_id"))
	if !ok {
		return "response=3&responsetext=Invalid+Billing+Id&response_code=300"
	}
	if code := charged.Decline; code != "" {
		d := &nmiSale{TransactionID: f.next("tx"), OrderID: form.Get("orderid"), Vault: v.ID, Amount: form.Get("amount"), Currency: strings.ToUpper(form.Get("currency")), Declined: code, At: f.now()}
		f.declined = append(f.declined, d)
		return fmt.Sprintf("response=2&responsetext=DECLINE&transactionid=%s&orderid=%s&response_code=%s", d.TransactionID, d.OrderID, code)
	}
	if f.duplicate > 0 {
		f.duplicate--
		return "response=3&responsetext=Duplicate+transaction+REFID%3A3187654321&response_code=300"
	}
	if f.duplicateOf(*charged, form.Get("amount")) || f.withinDupSeconds(*charged, form) {
		return "response=3&responsetext=Duplicate+transaction+REFID%3A3187654322&response_code=300"
	}
	amount, currency, scheduleID := form.Get("amount"), strings.ToUpper(form.Get("currency")), ""
	if form.Get("recurring") == "rebill_subscription" {
		schedule := f.schedules[form.Get("subscription_id")]
		if schedule == nil || schedule.Deleted || schedule.Vault != v.ID {
			return "response=3&responsetext=Invalid+subscription&response_code=300"
		}
		// NMI charges the saved schedule amount; rebill_subscription does not
		// take a request amount or advance the next regular billing date.
		amount, currency, scheduleID = schedule.Amount, "USD", schedule.ID
	}
	s := &nmiSale{TransactionID: f.next("tx"), OrderID: form.Get("orderid"), Vault: v.ID, BillingID: form.Get("billing_id"), Amount: amount, ScheduleID: scheduleID,
		Currency: currency, InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator"),
		Initial: form.Get("initial_transaction_id"), Card: *charged, At: f.now()}
	if s.BillingID == "" {
		s.BillingID = v.BillingID
	}
	if f.hide > 0 {
		f.hide--
		s.Hidden = true
	}
	f.sales = append(f.sales, s)
	f.remember(*charged, form.Get("amount"))
	return fmt.Sprintf("response=1&responsetext=SUCCESS&authcode=123456&transactionid=%s&orderid=%s&response_code=100", s.TransactionID, s.OrderID)
}

// addSubscription is a schedule-only Direct Post enrollment on a stored plan.
func (f *nmiFake) addSubscription(form url.Values) string {
	plan, v := f.plans[form.Get("plan_id")], f.vaults[form.Get("customer_vault_id")]
	if plan == nil || v == nil {
		return "response=3&responsetext=Invalid+plan+or+vault&response_code=300"
	}
	next, _ := time.Parse("20060102", form.Get("start_date"))
	s := &nmiSchedule{ID: f.next("rsub"), Vault: v.ID, Plan: plan["id"].(string), Amount: plan["plan_amount"].(string), Order: form.Get("orderid"), Days: plan["day_frequency"].(string), NextBilling: next}
	f.schedules[s.ID] = s
	return fmt.Sprintf("response=1&responsetext=Subscription+added&subscription_id=%s&orderid=%s&response_code=100", s.ID, form.Get("orderid"))
}

func (f *nmiFake) search(orderID, transactionID, scheduleID, vault string) string {
	var b strings.Builder
	b.WriteString("<nm_response>")
	for _, s := range append(append([]*nmiSale{}, f.sales...), f.declined...) {
		if s.Hidden || (orderID != "" && s.OrderID != orderID) || (transactionID != "" && s.TransactionID != transactionID) || (scheduleID != "" && s.ScheduleID != scheduleID) || (vault != "" && s.Vault != vault) {
			continue
		}
		success, code := "1", "100"
		if s.Declined != "" {
			success, code = "0", s.Declined
		}
		fmt.Fprintf(&b, "<transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><customer_vault_id>%s</customer_vault_id><currency>%s</currency><action><amount>%s</amount><action_type>sale</action_type><success>%s</success><response_code>%s</response_code><date>%s</date></action></transaction>",
			s.TransactionID, s.OrderID, s.Vault, s.Currency, s.Amount, success, code, s.At.Format("20060102150405"))
	}
	for _, v := range f.validations {
		order := v.Form.Get("orderid")
		if (orderID != "" && order != orderID) || (transactionID != "" && v.TransactionID != transactionID) || scheduleID != "" || (vault != "" && v.Vault != vault) {
			continue
		}
		success, code := "1", "100"
		if !v.Approved {
			success, code = "0", v.Card.Decline
		}
		fmt.Fprintf(&b, "<transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><customer_vault_id>%s</customer_vault_id><currency>USD</currency><action><amount>0.00</amount><action_type>validate</action_type><success>%s</success><response_code>%s</response_code><date>%s</date></action></transaction>",
			v.TransactionID, order, v.Vault, success, code, v.At.Format("20060102150405"))
	}
	b.WriteString("</nm_response>")
	return b.String()
}

// setDecline changes the issuer's answer for the card in every vault
// ending in last4.
func (f *nmiFake) setDecline(last4, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.vaults {
		if v.Card.Last4 == last4 {
			v.Card.Decline = code
		}
		for i := range v.Extra {
			if v.Extra[i].Card.Last4 == last4 {
				v.Extra[i].Card.Decline = code
			}
		}
	}
}

// loseSubmissions makes the next n sales fail in transit, never reaching
// the gateway.
func (f *nmiFake) loseSubmissions(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lose = n
}

// dropResponses makes the gateway process the next n sales and lose each
// answer in transit.
func (f *nmiFake) dropResponses(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drop = n
}

// queryUnavailable makes the Query API fail.
func (f *nmiFake) queryUnavailable(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryDown = down
}

// refuseDuplicates makes the next n sales trip NMI's duplicate check.
func (f *nmiFake) refuseDuplicates(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.duplicate = n
}

// ledger is the gateway's approved sales for a vault.
func (f *nmiFake) ledger(vault string) []ledgerEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ledgerEntry
	for _, s := range f.sales {
		if s.Declined == "" && (vault == "" || s.Vault == vault) {
			out = append(out, ledgerEntry{ID: s.TransactionID, Method: s.Card.Last4, Amount: centsOf(s.Amount), Refunded: s.RefundedCents})
		}
	}
	return out
}

func (f *nmiFake) saleAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.attempts)
}

func (f *nmiFake) lastSale() *nmiSale {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sales) == 0 {
		return nil
	}
	return f.sales[len(f.sales)-1]
}

func (f *nmiFake) unexpected() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.odd...)
	sort.Strings(out)
	return out
}

// legacyVault is a vault created by a legacy system.
func (f *nmiFake) legacyVault(c card) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := &nmiVault{ID: f.next("legacyvault"), BillingID: f.next("bill"), Card: c}
	f.vaults[v.ID] = v
	return v.ID
}

func (f *nmiFake) billingOf(vault string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vaults[vault].BillingID
}

// legacySchedule is a provider-owned plan subscription, billed by NMI.
func (f *nmiFake) legacySchedule(vault, plan, amount string, next time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &nmiSchedule{ID: f.next("rsub"), Vault: vault, Plan: plan, Amount: amount, NextBilling: next}
	f.schedules[s.ID] = s
	return s.ID
}

// legacySale records a charge NMI made for a schedule, as its recurring
// engine does: the schedule's order, the vault's card.
func (f *nmiFake) legacySale(vault, amount string, at time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &nmiSale{TransactionID: f.next("tx"), OrderID: "legacy-order", Vault: vault, BillingID: f.vaults[vault].BillingID, Amount: amount, Currency: "USD", Card: f.vaults[vault].Card, At: at}
	f.sales = append(f.sales, s)
	return s.TransactionID
}

// providerRenew is NMI's recurring engine charging a schedule; a failed
// charge advances to the next regular date without retrying the failed period.
func (f *nmiFake) providerRenew(id string, paid bool) (sale *nmiSale, declined bool) {
	f.mu.Lock()
	s := f.schedules[id]
	f.mu.Unlock()
	if !paid {
		f.mu.Lock()
		defer f.mu.Unlock()
		sale = &nmiSale{TransactionID: f.next("declined"), ScheduleID: id, Vault: s.Vault, BillingID: f.vaults[s.Vault].BillingID, Amount: s.Amount, Currency: "USD", At: s.NextBilling, Declined: "202"}
		f.sales = append(f.sales, sale)
		s.NextBilling = s.advance(s.NextBilling)
		return sale, true
	}
	tx := f.legacySale(s.Vault, s.Amount, s.NextBilling)
	f.mu.Lock()
	defer f.mu.Unlock()
	s.NextBilling = s.advance(s.NextBilling)
	sale = f.saleByID(tx)
	sale.ScheduleID = id
	return sale, false
}

func (f *nmiFake) providerCancel(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schedules[id].Deleted = true
}

func (f *nmiFake) scheduleLive(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.schedules[id].Deleted
}

// advance is the schedule's next regular billing date after at.
func (s *nmiSchedule) advance(at time.Time) time.Time {
	if months, _ := strconv.Atoi(s.Months); months > 0 {
		return at.AddDate(0, months, 0)
	}
	days, _ := strconv.Atoi(s.Days)
	if days <= 0 {
		days = 30
	}
	return at.AddDate(0, 0, days)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// updateSubscription is Direct Post recurring=update_subscription: a new
// vault (payment source) or a new schedule amount.
//
// NMI applies the change to future recurring charges only: the next billing
// date never moves.
func (f *nmiFake) updateSubscription(form url.Values) string {
	s := f.schedules[form.Get("subscription_id")]
	if s == nil || s.Deleted {
		return "response=3&responsetext=Invalid+subscription&response_code=300"
	}
	if f.failUpdates > 0 {
		f.failUpdates--
		return "response=3&responsetext=Subscription+update+unavailable&response_code=300"
	}
	if vault := form.Get("customer_vault_id"); vault != "" {
		if f.vaults[vault] == nil {
			return "response=3&responsetext=Invalid+Customer+Vault+Id&response_code=300"
		}
		s.Vault = vault
	}
	// As NMI does: a schedule on a named plan changes only by switching to
	// another named plan (amount and cadence follow the plan; the next
	// billing date stays). plan_amount and frequencies are accepted but
	// ignored there; a custom schedule applies them.
	if planID := form.Get("plan_id"); planID != "" {
		plan := f.plans[planID]
		if plan == nil {
			return "response=3&responsetext=Invalid+plan&response_code=300"
		}
		s.Plan, s.Amount, s.Custom = planID, fmt.Sprint(plan["plan_amount"]), false
		s.Days, s.Months = fmt.Sprint(plan["day_frequency"]), ""
		if m := fmt.Sprint(plan["month_frequency"]); m != "" && m != "0" && m != "<nil>" {
			s.Days, s.Months = "", m
		}
	} else if s.Custom {
		if amount := form.Get("plan_amount"); amount != "" {
			s.Amount = amount
		}
		if days := form.Get("day_frequency"); days != "" {
			s.Days, s.Months = days, ""
		}
		if months := form.Get("month_frequency"); months != "" {
			s.Months, s.Days = months, ""
		}
	}
	return fmt.Sprintf("response=1&responsetext=Subscription+updated&subscription_id=%s&response_code=100", s.ID)
}

// legacyPlan registers a plan created at NMI by the legacy system.
func (f *nmiFake) legacyPlan(id, amount string, days, months int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans[id] = obj{"object": "plan", "id": id, "plan_name": "Legacy " + id, "plan_amount": amount, "plan_payments": "0", "day_frequency": strconv.Itoa(days), "month_frequency": strconv.Itoa(months)}
}

// legacyScheduleEvery is a provider-owned schedule on a days or months cadence.
func (f *nmiFake) legacyScheduleEvery(vault, plan, amount string, days, months int, next time.Time) string {
	id := f.legacySchedule(vault, plan, amount, next)
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.schedules[id]
	s.Days, s.Months = "", ""
	if months > 0 {
		s.Months = strconv.Itoa(months)
	} else {
		s.Days = strconv.Itoa(days)
	}
	return id
}

// addCard adds a further card to a vault and returns its billing id.
func (f *nmiFake) addCard(vault string, c card) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.vaults[vault]
	b := nmiBilling{ID: f.next("bill"), Card: c}
	v.Extra = append(v.Extra, b)
	return b.ID
}

// removeVault deletes a vault at NMI outside OpenRails.
func (f *nmiFake) removeVault(vault string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vaults, vault)
}

// schedule returns a copy of a schedule's current state.
func (f *nmiFake) scheduleState(id string) nmiSchedule {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.schedules[id]
}

// editSchedule changes a schedule at NMI outside OpenRails.
func (f *nmiFake) editSchedule(id string, edit func(*nmiSchedule)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f.schedules[id])
}

// scheduleSale records a successful charge NMI's recurring engine made for
// a schedule at a time, without advancing it (for backdated books).
func (f *nmiFake) scheduleSale(id string, at time.Time) *nmiSale {
	f.mu.Lock()
	s := f.schedules[id]
	f.mu.Unlock()
	tx := f.legacySale(s.Vault, s.Amount, at)
	f.mu.Lock()
	defer f.mu.Unlock()
	sale := f.saleByID(tx)
	sale.ScheduleID = id
	return sale
}

// callsTo is the journal of provider mutations whose path starts with
// prefix (a v5 path such as "/subscriptions/rsub1", or "transact.php"
// narrowed by a Direct Post form field).
func (f *nmiFake) callsTo(method, prefix string, form func(url.Values) bool) []providerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []providerCall
	for _, c := range f.writes {
		if c.Method == method && strings.HasPrefix(c.Path, prefix) && (form == nil || form(c.Form)) {
			out = append(out, c)
		}
	}
	return out
}

// deletesOf counts DELETE requests for one schedule.
func (f *nmiFake) deletesOf(id string) int {
	return len(f.callsTo(http.MethodDelete, "/subscriptions/"+id, nil))
}

// validate is Direct Post type=validate: a no-funds card verification. The
// issuer refuses a card it would never honor; a funds decline (202/203)
// still verifies.
func (f *nmiFake) validate(form url.Values) string {
	v := f.vaults[form.Get("customer_vault_id")]
	if v == nil {
		return "response=3&responsetext=Invalid+Customer+Vault+Id&response_code=300"
	}
	c, ok := v.cardFor(form.Get("billing_id"))
	if !ok {
		return "response=3&responsetext=Invalid+Billing+Id&response_code=300"
	}
	if f.duplicateOf(*c, "0.00") {
		return "response=3&responsetext=Duplicate+transaction+REFID%3A3187654323&response_code=300"
	}
	id := f.next("validate")
	approved := c.Decline == "" || c.Decline == "202" || c.Decline == "203"
	if approved {
		f.remember(*c, "0.00")
	}
	f.validations = append(f.validations, nmiValidation{TransactionID: id, Vault: v.ID, BillingID: form.Get("billing_id"), Card: *c, Approved: approved, Form: form, At: f.now()})
	if !approved {
		return fmt.Sprintf("response=2&responsetext=DECLINE&transactionid=%s&response_code=%s", id, c.Decline)
	}
	return fmt.Sprintf("response=1&responsetext=VALIDATED&transactionid=%s&response_code=100", id)
}

type nmiValidation struct {
	TransactionID, Vault, BillingID string
	Card                            card
	Approved                        bool
	Form                            url.Values
	At                              time.Time
}

// validationOf is the approved card verification of a vault, or nil.
func (f *nmiFake) validationOf(vault string) *nmiValidation {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.validations {
		if f.validations[i].Vault == vault {
			return &f.validations[i]
		}
	}
	return nil
}

// failScheduleUpdates makes the next n update_subscription requests fail.
func (f *nmiFake) failScheduleUpdates(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failUpdates = n
}

// scheduleUpdates is the journal of update_subscription requests for a schedule.
func (f *nmiFake) scheduleUpdates(id string) []providerCall {
	return f.callsTo(http.MethodPost, "transact.php", func(v url.Values) bool {
		return v.Get("recurring") == "update_subscription" && v.Get("subscription_id") == id
	})
}

// customSchedule makes a schedule a custom-amount one (no named plan), as a
// legacy system that set plan_amount directly left it.
func (f *nmiFake) customSchedule(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.schedules[id]
	s.Custom, s.Plan = true, "custom-"+id
}

type nmiRecent struct {
	card   card
	amount string
	at     time.Time
}

// duplicateWindow turns on NMI's duplicate check against now's clock.
func (f *nmiFake) duplicateWindow(now func() time.Time, window time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dupNow, f.dupWindow = now, window
}

func (f *nmiFake) duplicateOf(c card, amount string) bool {
	if f.dupWindow <= 0 {
		return false
	}
	now := f.dupNow()
	for _, r := range f.recent {
		if r.card.Brand == c.Brand && r.card.Last4 == c.Last4 && r.amount == amount && now.Sub(r.at) < f.dupWindow {
			return true
		}
	}
	return false
}

func (f *nmiFake) remember(c card, amount string) {
	if f.dupWindow > 0 {
		f.recent = append(f.recent, nmiRecent{card: c, amount: amount, at: f.dupNow()})
	}
}

// withinDupSeconds is NMI's per-request duplicate check: dup_seconds refuses
// a sale matching an approved sale of the same card and amount within that
// many seconds, whether or not searches show it yet. Test vaults share card
// numbers, so a card is its vault's.
func (f *nmiFake) withinDupSeconds(c card, form url.Values) bool {
	window, _ := strconv.Atoi(form.Get("dup_seconds"))
	if window <= 0 {
		return false
	}
	for _, s := range f.sales {
		if s.Declined == "" && s.Vault == form.Get("customer_vault_id") && s.Card.Brand == c.Brand && s.Card.Last4 == c.Last4 && s.Amount == form.Get("amount") && f.now().Sub(s.At) <= time.Duration(window)*time.Second {
			return true
		}
	}
	return false
}

// hideSales makes the next n approved sales invisible to the Query API, as
// its indexing lag does, until reveal.
func (f *nmiFake) hideSales(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hide = n
}

func (f *nmiFake) reveal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sales {
		s.Hidden = false
	}
}

// approveUnder records an approved sale NMI shows under order on vault's
// current card, outside any request OpenRails made.
func (f *nmiFake) approveUnder(order, vault, amount string) *nmiSale {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.vaults[vault]
	s := &nmiSale{TransactionID: f.next("tx"), OrderID: order, Vault: vault, BillingID: v.BillingID, Amount: amount, Currency: "USD", Card: v.Card, At: f.now()}
	f.sales = append(f.sales, s)
	return s
}

// lastDeclined is the most recent declined sale request.
func (f *nmiFake) lastDeclined() *nmiSale {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.declined) == 0 {
		return nil
	}
	return f.declined[len(f.declined)-1]
}
