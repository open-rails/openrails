package nmimock

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type obj = map[string]any

func (m *Mock) serveV5(rec *httptest.ResponseRecorder, method, path string, q url.Values, body []byte) {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	if method != http.MethodGet {
		m.calls = append(m.calls, Call{Method: method, Path: path, Body: append([]byte(nil), body...)})
	} else if len(seg) > 1 {
		m.reads["v5:"+seg[0]+"/{id}"]++
	} else {
		m.reads["v5:"+seg[0]]++
	}
	status, out := m.v5(method, seg, q, body)
	raw, _ := json.Marshal(out)
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(status)
	_, _ = rec.Write(raw)
}

func v5NotFound() (int, any) {
	return http.StatusNotFound, obj{"type": "notFound", "error_code": "E_NOT_FOUND", "message": "not found"}
}

func v5Invalid(message string) (int, any) {
	return http.StatusBadRequest, obj{"type": "invalid", "error_code": "E_INVALID", "message": message}
}

type paymentDetails struct {
	PaymentDetails struct {
		PaymentToken string `json:"payment_token"`
	} `json:"payment_details"`
}

// cardOf resolves a Collect.js token: a Tokenize token names its card; any
// other token ending in four digits is a visa with those last four.
func (m *Mock) cardOf(token string) (Card, bool) {
	if c, ok := m.tokens[token]; ok {
		return c, true
	}
	token = strings.TrimSpace(token)
	if len(token) < 4 {
		return Card{}, false
	}
	last4 := token[len(token)-4:]
	if _, err := strconv.Atoi(last4); err != nil {
		return Card{}, false
	}
	c := Card{Brand: "visa", Last4: last4}
	if last4 == DeclineLast4 {
		c.Decline = "202"
	}
	return c, true
}

// page applies per_page and a decimal-offset cursor.
func page[T any](items []T, q url.Values) ([]T, string, bool) {
	from, _ := strconv.Atoi(q.Get("cursor"))
	from = min(max(from, 0), len(items))
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		return items[from:], "", false
	}
	to := min(from+per, len(items))
	if to == len(items) {
		return items[from:to], "", false
	}
	return items[from:to], strconv.Itoa(to), true
}

func (m *Mock) v5(method string, seg []string, q url.Values, body []byte) (int, any) {
	var in struct {
		Billing json.RawMessage `json:"billing"`
		Amount  json.RawMessage `json:"amount"`
	}
	_ = json.Unmarshal(body, &in)
	token := func() (Card, bool) {
		var one paymentDetails
		var many []paymentDetails
		t := ""
		if json.Unmarshal(in.Billing, &one) == nil {
			t = one.PaymentDetails.PaymentToken
		} else if json.Unmarshal(in.Billing, &many) == nil && len(many) > 0 {
			t = many[0].PaymentDetails.PaymentToken
		}
		return m.cardOf(t)
	}
	switch {
	case seg[0] == "customers" && len(seg) == 1 && method == http.MethodPost:
		c, ok := token()
		if !ok {
			return v5Invalid("invalid payment token")
		}
		if c.Decline == "vault" {
			m.refusedSaves++
			return v5Invalid("card refused")
		}
		v := &Vault{ID: m.next("vault"), BillingID: m.next("bill"), Card: c}
		m.vaults[v.ID] = v
		return http.StatusOK, m.customer(v)
	case seg[0] == "customers" && len(seg) == 1 && method == http.MethodGet:
		data := []obj{}
		for _, id := range sortedKeys(m.vaults) {
			if want := q.Get("id"); want == "" || want == id {
				data = append(data, m.customer(m.vaults[id]))
			}
		}
		data, next, more := page(data, q)
		return http.StatusOK, obj{"customers": data, "next_cursor": next, "has_more": more}
	case seg[0] == "customers" && len(seg) == 3 && seg[2] == "billing" && method == http.MethodPost:
		v, ok := m.vaults[seg[1]]
		if !ok {
			return v5NotFound()
		}
		var one paymentDetails
		_ = json.Unmarshal(body, &one)
		t := one.PaymentDetails.PaymentToken
		c, ok := m.cardOf(t)
		if !ok {
			return v5Invalid("invalid payment token")
		}
		delete(m.tokens, t)
		b := Billing{ID: m.next("bill"), Card: c}
		v.Extra = append(v.Extra, b)
		return http.StatusOK, obj{"object": "billing", "id": b.ID}
	case seg[0] == "customers" && len(seg) == 4 && seg[2] == "billing" && method == http.MethodDelete:
		v, ok := m.vaults[seg[1]]
		if !ok || !v.removeBilling(seg[3]) {
			return v5Invalid("billing entry cannot be removed")
		}
		return http.StatusOK, m.customer(v)
	case seg[0] == "customers" && len(seg) == 2:
		v, ok := m.vaults[seg[1]]
		if !ok {
			return v5NotFound()
		}
		if method == http.MethodPatch {
			if c, ok := token(); ok {
				v.Card = c
			}
		}
		if method == http.MethodDelete {
			delete(m.vaults, v.ID)
		}
		return http.StatusOK, m.customer(v)
	case seg[0] == "payments" && len(seg) == 2 && seg[1] == "auth":
		id := m.next("probe")
		m.probes[id] = true
		return http.StatusOK, obj{"object": "transaction", "id": id, "response": "1", "response_code": "100", "response_text": "SUCCESS"}
	case seg[0] == "payments" && len(seg) == 3 && seg[2] == "void":
		if bad := m.void(seg[1]); bad != "" {
			return v5Invalid(bad)
		}
		return http.StatusOK, obj{"object": "transaction", "id": seg[1], "response": "1", "response_code": "100", "response_text": "Transaction Void Successful"}
	case seg[0] == "payments" && len(seg) == 3 && seg[2] == "refund":
		if m.saleByID(seg[1]) == nil {
			return v5NotFound()
		}
		amount := ""
		if len(in.Amount) > 0 {
			var n json.Number
			if json.Unmarshal(in.Amount, &n) != nil {
				return v5Invalid("invalid amount")
			}
			amount = n.String()
		}
		id, bad := m.refund(seg[1], amount)
		if bad != "" {
			return v5Invalid(bad)
		}
		r := m.refunds[len(m.refunds)-1]
		return http.StatusOK, obj{"object": "transaction", "id": id, "response": "1", "response_code": "100", "response_text": "SUCCESS", "amount": decimalCents(r.Cents)}
	case seg[0] == "payments" && len(seg) == 2 && method == http.MethodGet:
		for _, r := range m.refunds {
			if r.ID == seg[1] {
				amount := decimalCents(r.Cents)
				return http.StatusOK, obj{"object": "transaction", "id": r.ID, "response": "1", "response_code": "100", "response_text": "SUCCESS",
					"amount": amount, "currency": r.Sale.Currency, "customer_vault_id": r.Sale.Vault,
					"actions": []obj{{"id": r.ID, "type": "refund", "amount": amount, "success": true, "response": "1", "response_code": "100"}}}
			}
		}
		s := m.saleByID(seg[1])
		if s == nil {
			return v5NotFound()
		}
		response, code, text := "1", "100", "SUCCESS"
		if !s.Approved() {
			response, code, text = "2", s.Declined, "DECLINE"
		}
		actions := []obj{{"id": s.TransactionID, "type": "sale", "amount": s.Amount, "success": s.Approved(), "response": response, "response_code": code}}
		for _, r := range m.refunds {
			if r.Sale == s {
				actions = append(actions, obj{"id": r.ID, "type": "refund", "amount": decimalCents(r.Cents), "success": true, "response": "1", "response_code": "100"})
			}
		}
		return http.StatusOK, obj{"object": "transaction", "id": s.TransactionID, "response": response, "response_code": code, "response_text": text,
			"amount": s.Amount, "currency": s.Currency, "customer_vault_id": s.Vault, "actions": actions}
	case seg[0] == "plans" && len(seg) == 1 && method == http.MethodGet:
		data := []obj{}
		for _, id := range sortedKeys(m.plans) {
			data = append(data, planJSON(*m.plans[id]))
		}
		data, next, more := page(data, q)
		return http.StatusOK, obj{"plans": data, "next_cursor": next, "has_more": more}
	case seg[0] == "plans" && len(seg) == 1 && method == http.MethodPost:
		var p struct {
			ID           string          `json:"id"`
			Name         string          `json:"plan_name"`
			Amount       json.RawMessage `json:"plan_amount"`
			DayFrequency int             `json:"day_frequency"`
		}
		if json.Unmarshal(body, &p) != nil || strings.TrimSpace(p.ID) == "" || m.plans[p.ID] != nil {
			return v5Invalid("invalid or duplicate plan")
		}
		m.plans[p.ID] = &Plan{ID: p.ID, Name: p.Name, Amount: strings.Trim(string(p.Amount), `"`), Days: p.DayFrequency}
		return http.StatusOK, planJSON(*m.plans[p.ID])
	case seg[0] == "plans" && len(seg) == 2 && method == http.MethodGet:
		if p, ok := m.plan(seg[1]); ok {
			return http.StatusOK, planJSON(p)
		}
		return v5NotFound()
	case seg[0] == "subscriptions" && len(seg) == 1 && method == http.MethodGet:
		data := []obj{}
		for _, id := range sortedKeys(m.schedules) {
			if s := m.schedules[id]; !s.Deleted {
				data = append(data, m.schedule(s))
			}
		}
		data, next, more := page(data, q)
		return http.StatusOK, obj{"subscriptions": data, "next_cursor": next, "has_more": more}
	case seg[0] == "subscriptions" && len(seg) == 2 && (method == http.MethodGet || method == http.MethodDelete):
		s, ok := m.schedules[seg[1]]
		if !ok {
			return v5NotFound()
		}
		if method == http.MethodDelete {
			s.Deleted = true
		}
		return http.StatusOK, m.schedule(s)
	}
	m.odd = append(m.odd, method+" v5/"+strings.Join(seg, "/"))
	return v5NotFound()
}

func (m *Mock) customer(v *Vault) obj {
	details := func(c Card) obj {
		d := obj{"card_number": maskedNumber(c), "card_exp": "1235", "card_type": c.Brand}
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

func planJSON(p Plan) obj {
	name := p.Name
	return obj{"object": "plan", "id": p.ID, "plan_name": name, "plan_amount": p.Amount, "plan_payments": "0",
		"day_frequency": itoa(p.Days), "month_frequency": itoa(p.Months), "day_of_month": ""}
}

func (m *Mock) schedule(s *Schedule) obj {
	days, months := s.Days, s.Months
	if days == 0 && months == 0 {
		days = 30
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
		name = s.Plan
		if p, ok := m.plan(s.Plan); ok && p.Name != "" {
			name = p.Name
		}
	}
	return obj{"object": "subscription", "id": s.ID, "customer_vault_id": s.Vault, "delayed_condition": condition, "paused_subscription": paused,
		"amount": s.Amount, "next_billing_date": s.NextBilling.Format("2006-01-02"),
		"plan": obj{"id": s.Plan, "plan_name": name, "plan_amount": s.Amount, "day_frequency": itoa(days), "month_frequency": itoa(months), "plan_payments": "0"}}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
