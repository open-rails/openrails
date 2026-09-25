// Package nmifake is a loopback NMI gateway for sandbox tests: Customer Vault
// (v5), Direct Post validate/sale and the Query reads OpenRails makes. Point
// provider_sandbox.nmi_gateway_url at URL(). Any Collect.js token is accepted;
// its last four digits name the card, and DeclineLast4 makes that card's
// sales decline (verification still passes, as for insufficient funds).
package nmifake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DeclineLast4 is the card ending a decline: "insufficient funds" (202).
const DeclineLast4 = "0002"

type vault struct {
	ID, BillingID, Last4 string
}

type Sale struct {
	TransactionID, OrderID, Vault, Amount, Currency, Code string
	Approved                                              bool
	At                                                    time.Time
}

type Gateway struct {
	server *httptest.Server
	mu     sync.Mutex
	seq    int
	vaults map[string]*vault
	sales  []Sale
	plans  map[string]obj
}

func New() *Gateway {
	g := NewUnstarted()
	g.server = httptest.NewServer(g)
	return g
}

// NewUnstarted is a gateway the caller serves (the sandbox CLI command).
func NewUnstarted() *Gateway { return &Gateway{vaults: map[string]*vault{}, plans: map[string]obj{}} }

// AddPlan stores a Recurring Plan billing amount (decimal, e.g. "23.00")
// every dayFrequency days, open-ended: a plan the account already has.
func (g *Gateway) AddPlan(id, amount string, dayFrequency int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.addPlan(id, id, amount, dayFrequency)
}

func (g *Gateway) addPlan(id, name, amount string, dayFrequency int) obj {
	p := obj{"object": "plan", "id": id, "plan_name": name, "plan_amount": amount, "plan_payments": "0",
		"day_frequency": fmt.Sprint(dayFrequency), "month_frequency": "", "day_of_month": ""}
	g.plans[id] = p
	return p
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.serve(w, r) }

func (g *Gateway) URL() string { return g.server.URL }
func (g *Gateway) Close()      { g.server.Close() }

// Sales lists every sale attempt, approved or declined.
func (g *Gateway) Sales() []Sale {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Sale(nil), g.sales...)
}

// Vaults is the number of stored cards.
func (g *Gateway) Vaults() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.vaults)
}

func (g *Gateway) next(prefix string) string {
	g.seq++
	return fmt.Sprintf("%s%08d", prefix, g.seq)
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	defer g.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/customers") || strings.HasPrefix(r.URL.Path, "/payments") || strings.HasPrefix(r.URL.Path, "/plans") {
		status, out := g.v5(r.Method, strings.Split(strings.Trim(r.URL.Path, "/"), "/"), body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	form, _ := url.ParseQuery(string(body))
	for k, v := range r.URL.Query() {
		form[k] = v
	}
	switch {
	case form.Get("report_type") == "test_mode_status":
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, "<nm_response><test_mode_status>enabled</test_mode_status></nm_response>")
	case form.Get("report_type") == "transaction":
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, g.search(form))
	case form.Get("type") == "validate":
		if g.vaults[form.Get("customer_vault_id")] == nil {
			_, _ = io.WriteString(w, "response=3&responsetext=Invalid+Customer+Vault+Id&response_code=300")
			return
		}
		_, _ = fmt.Fprintf(w, "response=1&responsetext=VALIDATED&transactionid=%s&response_code=100", g.next("validate"))
	case form.Get("type") == "sale":
		_, _ = io.WriteString(w, g.sale(form))
	default:
		http.NotFound(w, r)
	}
}

func (g *Gateway) sale(form url.Values) string {
	v := g.vaults[form.Get("customer_vault_id")]
	if v == nil {
		return "response=3&responsetext=Invalid+Customer+Vault+Id&response_code=300"
	}
	s := Sale{TransactionID: g.next("tx"), OrderID: form.Get("orderid"), Vault: v.ID, Amount: form.Get("amount"), Currency: strings.ToUpper(form.Get("currency")), Code: "100", Approved: true, At: time.Now().UTC()}
	if v.Last4 == DeclineLast4 {
		s.Code, s.Approved = "202", false
	}
	g.sales = append(g.sales, s)
	if !s.Approved {
		return fmt.Sprintf("response=2&responsetext=DECLINE&transactionid=%s&orderid=%s&response_code=%s", s.TransactionID, s.OrderID, s.Code)
	}
	return fmt.Sprintf("response=1&responsetext=SUCCESS&authcode=123456&transactionid=%s&orderid=%s&response_code=100", s.TransactionID, s.OrderID)
}

func (g *Gateway) search(form url.Values) string {
	var b strings.Builder
	b.WriteString("<nm_response>")
	for _, s := range g.sales {
		if (form.Get("order_id") != "" && s.OrderID != form.Get("order_id")) || (form.Get("transaction_id") != "" && s.TransactionID != form.Get("transaction_id")) || (form.Get("customer_vault_id") != "" && s.Vault != form.Get("customer_vault_id")) {
			continue
		}
		success := "1"
		if !s.Approved {
			success = "0"
		}
		fmt.Fprintf(&b, "<transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><customer_vault_id>%s</customer_vault_id><currency>%s</currency><action><amount>%s</amount><action_type>sale</action_type><success>%s</success><response_code>%s</response_code><date>%s</date></action></transaction>",
			s.TransactionID, s.OrderID, s.Vault, s.Currency, s.Amount, success, s.Code, s.At.Format("20060102150405"))
	}
	b.WriteString("</nm_response>")
	return b.String()
}

type obj = map[string]any

func (g *Gateway) customer(v *vault) obj {
	return obj{"object": "customer", "id": v.ID, "created": "2026-01-01T00:00:00Z", "billing": []obj{{
		"id": v.BillingID, "priority": 1,
		"payment_details": obj{"card_number": "4xxxxxxxxxxx" + v.Last4, "card_exp": "1235", "card_type": "visa"},
	}}}
}

func (g *Gateway) v5(method string, seg []string, body []byte) (int, any) {
	notFound := obj{"type": "notFound", "error_code": "E_NOT_FOUND", "message": "not found"}
	switch {
	case seg[0] == "customers" && len(seg) == 1 && method == http.MethodPost:
		var in struct {
			Billing struct {
				PaymentDetails struct {
					PaymentToken string `json:"payment_token"`
				} `json:"payment_details"`
			} `json:"billing"`
		}
		_ = json.Unmarshal(body, &in)
		token := strings.TrimSpace(in.Billing.PaymentDetails.PaymentToken)
		if len(token) < 4 {
			return 400, obj{"type": "invalid", "message": "bad token"}
		}
		v := &vault{ID: g.next("vault"), BillingID: g.next("bill"), Last4: token[len(token)-4:]}
		g.vaults[v.ID] = v
		return 200, g.customer(v)
	case seg[0] == "customers" && len(seg) == 2:
		v, ok := g.vaults[seg[1]]
		if !ok {
			return 404, notFound
		}
		if method == http.MethodDelete {
			delete(g.vaults, v.ID)
		}
		return 200, g.customer(v)
	case seg[0] == "payments" && len(seg) == 2 && method == http.MethodGet:
		for _, s := range g.sales {
			if s.TransactionID == seg[1] {
				response := "1"
				if !s.Approved {
					response = "2"
				}
				return 200, obj{"object": "transaction", "id": s.TransactionID, "response": response, "response_code": s.Code, "amount": s.Amount, "currency": s.Currency, "customer_vault_id": s.Vault,
					"actions": []obj{{"id": s.TransactionID, "type": "sale", "amount": s.Amount, "success": s.Approved, "response": response, "response_code": s.Code}}}
			}
		}
		return 404, notFound
	case seg[0] == "plans" && len(seg) == 1 && method == http.MethodGet:
		out := make([]obj, 0, len(g.plans))
		for _, p := range g.plans {
			out = append(out, p)
		}
		return 200, obj{"plans": out, "has_more": false}
	case seg[0] == "plans" && len(seg) == 1 && method == http.MethodPost:
		// Raw fields: the fake stores the wire amount as sent, never as money.
		var in map[string]json.RawMessage
		var id, name string
		var days int
		if json.Unmarshal(body, &in) != nil || json.Unmarshal(in["id"], &id) != nil || strings.TrimSpace(id) == "" || g.plans[id] != nil {
			return 400, obj{"type": "invalid", "message": "bad or duplicate plan"}
		}
		_ = json.Unmarshal(in["plan_name"], &name)
		_ = json.Unmarshal(in["day_frequency"], &days)
		return 200, g.addPlan(id, name, strings.Trim(string(in["plan_amount"]), `"`), days)
	case seg[0] == "plans" && len(seg) == 2 && method == http.MethodGet:
		if p, ok := g.plans[seg[1]]; ok {
			return 200, p
		}
		return 404, notFound
	case seg[0] == "payments" && len(seg) == 2 && seg[1] == "auth":
		return 200, obj{"object": "transaction", "id": g.next("probe"), "response": "1", "response_code": "100", "response_text": "SUCCESS"}
	}
	return 404, notFound
}
