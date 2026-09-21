//go:build integration

package integrationharness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// NMISaleMode scripts how the loopback gateway answers a classic sale.
type NMISaleMode string

const (
	// NMISaleApprove records the sale and answers approved.
	NMISaleApprove NMISaleMode = "approve"
	// NMISaleUncertain records the sale (it landed at the provider) but
	// answers the processor-communication-error code, so the caller cannot
	// know: the #990 lost-response window.
	NMISaleUncertain NMISaleMode = "uncertain"
	// NMISaleDecline records nothing and answers declined.
	NMISaleDecline NMISaleMode = "decline"
)

// NMISale is one sale the gateway recorded.
type NMISale struct {
	OrderID       string
	TransactionID string
	Vault         string
	Amount        string
	Currency      string
	At            time.Time
}

// NMIEnrollment is one recurring enrollment (recurring=add_subscription) the
// gateway recorded.
type NMIEnrollment struct {
	SaleType                  string
	SaleAmount                string
	StoredCredentialIndicator string
	SubscriptionID            string
	OrderID                   string
	PONumber                  string
	FirstCharge               string
	Vault                     string
	Plan                      string
	Amount                    string
	Deleted                   bool
}

// FakeNMIGateway is a loopback NMI: the classic Direct Post sale and
// recurring enrollment, the classic Query API search, and the v5 exact
// transaction and subscription reads plus subscription delete — the wire
// paths invoice collection, checkout sales, tier upgrades and their verifiers
// use. A recorded sale is reported by search and exact read only while
// Visible, which models delayed provider visibility. Enrollments always
// approve. It is a fake provider: nothing here proves live NMI behavior.
type FakeNMIGateway struct {
	URL string

	server      *httptest.Server
	mu          sync.Mutex
	mode        NMISaleMode
	visible     bool
	sales       []NMISale
	attempts    int
	enrollments []NMIEnrollment
	plans       map[string]nmi.V5Plan
}

// NewFakeNMIGateway starts the gateway approving and visible. Point a runtime
// at it with config.ProviderSandbox{NMIGatewayURL: g.URL}.
func NewFakeNMIGateway(t testing.TB) *FakeNMIGateway {
	t.Helper()
	g := &FakeNMIGateway{mode: NMISaleApprove, visible: true, plans: map[string]nmi.V5Plan{}}
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	g.URL = g.server.URL
	t.Cleanup(g.server.Close)
	return g
}

// DeclarePlan keeps provider schedule facts independent of enrollment's optional
// initial sale amount. A schedule-only command never creates a payment.
func (g *FakeNMIGateway) DeclarePlan(plan nmi.V5Plan) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.plans[plan.ID] = plan
}

func (g *FakeNMIGateway) SetMode(mode NMISaleMode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = mode
}

// SetVisible controls whether recorded sales are reported by search and exact
// reads.
func (g *FakeNMIGateway) SetVisible(visible bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.visible = visible
}

// Sales returns every sale request the gateway recorded, in order.
func (g *FakeNMIGateway) Sales() []NMISale {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]NMISale(nil), g.sales...)
}

// SaleCount is the number of sales the gateway recorded (declines record
// nothing).
func (g *FakeNMIGateway) SaleCount() int { return len(g.Sales()) }

// SaleAttempts counts every sale submission, declines included.
func (g *FakeNMIGateway) SaleAttempts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attempts
}

// Enrollments returns every recurring enrollment the gateway recorded, in
// order.
func (g *FakeNMIGateway) Enrollments() []NMIEnrollment {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]NMIEnrollment(nil), g.enrollments...)
}

// SaleForOrder returns the recorded sale carrying orderID.
func (g *FakeNMIGateway) SaleForOrder(orderID string) (NMISale, bool) {
	for _, sale := range g.Sales() {
		if sale.OrderID == orderID {
			return sale, true
		}
	}
	return NMISale{}, false
}

func (g *FakeNMIGateway) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/payments/") {
		g.serveExactRead(w, strings.TrimPrefix(r.URL.Path[strings.LastIndex(r.URL.Path, "/payments/"):], "/payments/"))
		return
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodDelete) && strings.Contains(r.URL.Path, "/subscriptions/") {
		g.serveSubscription(w, r.Method, strings.TrimPrefix(r.URL.Path[strings.LastIndex(r.URL.Path, "/subscriptions/"):], "/subscriptions/"))
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case r.Form.Get("recurring") == "add_subscription":
		g.serveEnrollment(w, r)
	case r.Form.Get("type") == "sale":
		g.serveSale(w, r)
	case r.Form.Get("report_type") == "transaction":
		g.serveSearch(w, r.Form.Get("order_id"))
	case r.Form.Get("report_type") == "recurring":
		g.serveEnrollmentReport(w, r.Form.Get("subscription_id"))
	default:
		http.Error(w, "unsupported fake NMI request", http.StatusNotImplemented)
	}
}

func (g *FakeNMIGateway) serveSale(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.attempts++
	if g.mode == NMISaleDecline {
		fmt.Fprint(w, "response=2&responsetext=DECLINED&response_code=200")
		return
	}
	sale := NMISale{
		OrderID:       r.Form.Get("orderid"),
		TransactionID: "tx-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		Vault:         r.Form.Get("customer_vault_id"),
		Amount:        r.Form.Get("amount"),
		Currency:      strings.ToUpper(r.Form.Get("currency")),
		At:            time.Now().UTC(),
	}
	g.sales = append(g.sales, sale)
	if g.mode == NMISaleUncertain {
		fmt.Fprint(w, "response=3&responsetext=Communication+error&response_code=421")
		return
	}
	fmt.Fprintf(w, "response=1&responsetext=SUCCESS&authcode=123456&transactionid=%s&orderid=%s&response_code=100", sale.TransactionID, sale.OrderID)
}

func (g *FakeNMIGateway) serveEnrollment(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	enrollment := NMIEnrollment{SaleType: r.Form.Get("type"), SaleAmount: r.Form.Get("amount"), StoredCredentialIndicator: r.Form.Get("stored_credential_indicator"),
		SubscriptionID: "rsub-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		OrderID:        r.Form.Get("orderid"),
		PONumber:       r.Form.Get("ponumber"),
		FirstCharge:    r.Form.Get("start_date"),
		Vault:          r.Form.Get("customer_vault_id"),
		Plan:           r.Form.Get("plan_id"),
		Amount:         r.Form.Get("amount"),
	}
	if plan, ok := g.plans[enrollment.Plan]; ok {
		enrollment.Amount = plan.PlanAmount
	}
	transaction := ""
	if r.Form.Get("type") == "sale" {
		g.attempts++
		if g.mode == NMISaleDecline {
			fmt.Fprint(w, "response=2&responsetext=DECLINED&response_code=200")
			return
		}
		transaction = "tx-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		g.sales = append(g.sales, NMISale{OrderID: enrollment.OrderID, TransactionID: transaction, Vault: enrollment.Vault, Amount: r.Form.Get("amount"), Currency: strings.ToUpper(r.Form.Get("currency")), At: time.Now().UTC()})
	}
	g.enrollments = append(g.enrollments, enrollment)
	if transaction != "" && g.mode == NMISaleUncertain {
		fmt.Fprint(w, "response=3&responsetext=Communication+error&response_code=421")
		return
	}
	fmt.Fprintf(w, "response=1&responsetext=SUCCESS&subscription_id=%s&transactionid=%s&response_code=100", enrollment.SubscriptionID, transaction)
}

// serveSubscription is the v5 subscription read (a live enrollment, else
// 404) and delete.
func (g *FakeNMIGateway) serveSubscription(w http.ResponseWriter, method, id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	for i := range g.enrollments {
		enrollment := &g.enrollments[i]
		if enrollment.SubscriptionID != id || enrollment.Deleted {
			continue
		}
		if method == http.MethodDelete {
			enrollment.Deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		start, err := time.Parse("20060102", enrollment.FirstCharge)
		if err != nil {
			http.Error(w, "fixture enrollment has invalid first charge date", http.StatusInternalServerError)
			return
		}
		plan := nmi.V5Plan{ID: enrollment.Plan, PlanAmount: enrollment.Amount, DayFrequency: "30", PlanPayments: "0"}
		if declared, ok := g.plans[enrollment.Plan]; ok {
			plan = declared
		}
		_ = json.NewEncoder(w).Encode(nmi.V5Subscription{
			Object: "subscription", ID: enrollment.SubscriptionID, CustomerVaultID: enrollment.Vault,
			DelayedCondition: "active", PausedSubscription: false, NextBillingDate: start.Format("2006-01-02"),
			Plan: &plan,
		})
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprint(w, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"subscription not found"}`)
}

func (g *FakeNMIGateway) serveEnrollmentReport(w http.ResponseWriter, id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, "<nm_response>")
	for _, enrollment := range g.enrollments {
		if enrollment.SubscriptionID == id && !enrollment.Deleted {
			start, err := time.Parse("20060102", enrollment.FirstCharge)
			if err == nil {
				fmt.Fprintf(w, `<subscription id="%s"><subscription_id>%s</subscription_id><plan><plan_id>%s</plan_id></plan><orderid>%s</orderid><ponumber>%s</ponumber><next_charge_date>%s</next_charge_date></subscription>`, id, id, enrollment.Plan, enrollment.OrderID, enrollment.PONumber, start.Format("2006-01-02"))
			}
		}
	}
	fmt.Fprint(w, "</nm_response>")
}

func (g *FakeNMIGateway) serveSearch(w http.ResponseWriter, orderID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b strings.Builder
	b.WriteString("<nm_response>")
	if g.visible {
		for _, sale := range g.sales {
			if orderID != "" && sale.OrderID != orderID {
				continue
			}
			fmt.Fprintf(&b, "<transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><currency>%s</currency><action><amount>%s</amount><action_type>sale</action_type><success>1</success><date>%s</date></action></transaction>",
				sale.TransactionID, sale.OrderID, sale.Currency, sale.Amount, sale.At.Format("20060102150405"))
		}
	}
	b.WriteString("</nm_response>")
	w.Header().Set("Content-Type", "text/xml")
	_, _ = fmt.Fprint(w, b.String())
}

func (g *FakeNMIGateway) serveExactRead(w http.ResponseWriter, transactionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if g.visible {
		for _, sale := range g.sales {
			if sale.TransactionID != transactionID {
				continue
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "transaction", "id": sale.TransactionID, "response": "1", "response_code": "100",
				"amount": sale.Amount, "currency": sale.Currency, "customer_vault_id": sale.Vault,
				"actions": []map[string]any{{"id": sale.TransactionID, "type": "sale", "amount": sale.Amount, "success": true}},
			})
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprint(w, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"transaction not found"}`)
}
