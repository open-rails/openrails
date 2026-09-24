//go:build greenfield && integration

package subscriptions_test

import (
	"encoding/json"
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
}

type nmiSale struct {
	TransactionID, OrderID, Vault, BillingID, Amount, Currency string
	InitiatedBy, Indicator, Initial                            string
	Card                                                       card
	RefundedCents                                              int64
	RefundIDs                                                  []string
	At                                                         time.Time
}

// nmiSchedule is a provider-owned recurring plan subscription.
type nmiSchedule struct {
	ID, Vault, Plan, Amount string
	NextBilling             time.Time
	Deleted                 bool
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
	writes    []providerCall
	gates     []*gate
	odd       []string
}

func newNMIFake() *nmiFake {
	return &nmiFake{tokens: map[string]card{}, vaults: map[string]*nmiVault{}, schedules: map[string]*nmiSchedule{}}
}

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
	f.mu.Unlock()
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
	case strings.HasSuffix(p, "/transact.php") && form.Get("type") == "sale":
		f.writes = append(f.writes, providerCall{Method: r.Method, Path: "transact.php", Form: form})
		_, _ = rec.WriteString(f.sale(form))
	case strings.HasSuffix(p, "/query.php") && form.Get("report_type") == "transaction":
		rec.Header().Set("Content-Type", "text/xml")
		_, _ = rec.WriteString(f.search(form.Get("order_id"), form.Get("transaction_id")))
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
	one := 1
	return obj{"object": "customer", "id": v.ID, "created": "2026-01-01T00:00:00Z", "billing": []obj{{"id": v.BillingID, "priority": &one,
		"payment_details": obj{"card_number": "4xxxxxxxxxxx" + v.Card.Last4, "card_exp": "1235", "card_type": v.Card.Brand}}}}
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
		v := &nmiVault{ID: f.next("vault"), BillingID: f.next("bill"), Card: c}
		f.vaults[v.ID] = v
		return 200, f.customer(v)
	case seg[0] == "customers" && len(seg) == 1 && method == http.MethodGet:
		var data []obj
		for _, v := range f.vaults {
			data = append(data, f.customer(v))
		}
		return 200, obj{"object": "list", "data": data}
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
		return 200, obj{"object": "transaction", "id": refundID, "response": "1", "response_code": "100", "response_text": "SUCCESS", "amount": fmt.Sprintf("%.2f", float64(cents)/100)}
	case seg[0] == "payments" && len(seg) == 2 && method == http.MethodGet:
		s := f.saleByID(seg[1])
		if s == nil {
			return nmiNotFound()
		}
		actions := []obj{{"id": s.TransactionID, "type": "sale", "amount": s.Amount, "success": true, "response": "1", "response_code": "100"}}
		if s.RefundedCents > 0 {
			actions = append(actions, obj{"id": s.TransactionID + "-r", "type": "refund", "amount": fmt.Sprintf("%.2f", float64(s.RefundedCents)/100), "success": true})
		}
		return 200, obj{"object": "transaction", "id": s.TransactionID, "response": "1", "response_code": "100", "amount": s.Amount, "currency": s.Currency, "customer_vault_id": s.Vault, "actions": actions}
	case seg[0] == "subscriptions" && len(seg) == 1:
		var data []obj
		for _, s := range f.schedules {
			if !s.Deleted {
				data = append(data, f.schedule(s))
			}
		}
		return 200, obj{"object": "list", "data": data}
	case seg[0] == "subscriptions" && len(seg) == 2:
		s, ok := f.schedules[seg[1]]
		if !ok || s.Deleted {
			return nmiNotFound()
		}
		if method == http.MethodDelete {
			s.Deleted = true
			return 204, nil
		}
		return 200, f.schedule(s)
	}
	f.odd = append(f.odd, method+" v5/"+strings.Join(seg, "/"))
	return nmiNotFound()
}

func (f *nmiFake) schedule(s *nmiSchedule) obj {
	return obj{"object": "subscription", "id": s.ID, "customer_vault_id": s.Vault, "delayed_condition": "active", "paused_subscription": false,
		"next_billing_date": s.NextBilling.Format("2006-01-02"), "plan": obj{"id": s.Plan, "plan_amount": s.Amount, "day_frequency": "30", "plan_payments": "0"}}
}

func centsOf(amount string) int64 {
	f, _ := strconv.ParseFloat(amount, 64)
	return int64(f*100 + 0.5)
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
	if code := v.Card.Decline; code != "" {
		return "response=2&responsetext=DECLINE&response_code=" + code
	}
	s := &nmiSale{TransactionID: f.next("tx"), OrderID: form.Get("orderid"), Vault: v.ID, BillingID: form.Get("billing_id"), Amount: form.Get("amount"),
		Currency: strings.ToUpper(form.Get("currency")), InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator"),
		Initial: form.Get("initial_transaction_id"), Card: v.Card, At: time.Now().UTC()}
	if s.BillingID == "" {
		s.BillingID = v.BillingID
	}
	f.sales = append(f.sales, s)
	return fmt.Sprintf("response=1&responsetext=SUCCESS&authcode=123456&transactionid=%s&orderid=%s&response_code=100", s.TransactionID, s.OrderID)
}

func (f *nmiFake) search(orderID, transactionID string) string {
	var b strings.Builder
	b.WriteString("<nm_response>")
	for _, s := range f.sales {
		if (orderID != "" && s.OrderID != orderID) || (transactionID != "" && s.TransactionID != transactionID) {
			continue
		}
		fmt.Fprintf(&b, "<transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><customer_vault_id>%s</customer_vault_id><currency>%s</currency><action><amount>%s</amount><action_type>sale</action_type><success>1</success><response_code>100</response_code><date>%s</date></action></transaction>",
			s.TransactionID, s.OrderID, s.Vault, s.Currency, s.Amount, s.At.Format("20060102150405"))
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
	}
}

// ledger is the gateway's approved sales for a vault.
func (f *nmiFake) ledger(vault string) []ledgerEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ledgerEntry
	for _, s := range f.sales {
		if vault == "" || s.Vault == vault {
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
