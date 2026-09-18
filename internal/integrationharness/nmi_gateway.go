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

// FakeNMIGateway is a loopback NMI: the classic Direct Post sale, the classic
// Query API search and the v5 exact transaction read — the three wire paths
// invoice collection, checkout sales and their verifiers use. A recorded sale
// is reported by search and exact read only while Visible, which models
// delayed provider visibility. It is a fake provider: nothing here proves live
// NMI behavior.
type FakeNMIGateway struct {
	URL string

	server   *httptest.Server
	mu       sync.Mutex
	mode     NMISaleMode
	visible  bool
	sales    []NMISale
	requests int
	// plans are the recurring subscriptions' plan amounts: a rebill sale
	// names the subscription and NMI charges its plan.
	plans map[string]plan
	// hold blocks sales in flight (the charger is mid-request at the
	// provider), so a claim the caller took is genuinely held meanwhile.
	hold chan struct{}
	held int
}

// HoldSales blocks every sale in flight until the returned release is called.
// A held sale is recorded first: it landed at the provider.
func (g *FakeNMIGateway) HoldSales() (release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	hold := make(chan struct{})
	g.hold = hold
	var once sync.Once
	return func() {
		g.mu.Lock()
		if g.hold == hold {
			g.hold = nil
		}
		g.mu.Unlock()
		once.Do(func() { close(hold) })
	}
}

// HeldSales is the number of sales blocked in flight right now.
func (g *FakeNMIGateway) HeldSales() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held
}

type plan struct{ amount, currency string }

// RegisterPlan records the plan amount ("12.00") and currency NMI charges
// when a rebill names subscriptionID.
func (g *FakeNMIGateway) RegisterPlan(subscriptionID, amount, currency string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.plans == nil {
		g.plans = map[string]plan{}
	}
	g.plans[subscriptionID] = plan{amount: amount, currency: strings.ToUpper(currency)}
}

// TamperSale rewrites the recorded sale carrying orderID, modeling a provider
// record that contradicts what was sent.
func (g *FakeNMIGateway) TamperSale(orderID string, mutate func(*NMISale)) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.sales {
		if g.sales[i].OrderID == orderID {
			mutate(&g.sales[i])
			return true
		}
	}
	return false
}

// NewFakeNMIGateway starts the gateway approving and visible. Point a runtime
// at it with config.ProviderSandbox{NMIGatewayURL: g.URL}.
func NewFakeNMIGateway(t testing.TB) *FakeNMIGateway {
	t.Helper()
	g := &FakeNMIGateway{mode: NMISaleApprove, visible: true}
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	g.URL = g.server.URL
	t.Cleanup(g.server.Close)
	return g
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

// SaleCount is the number of recorded sale receipts (declines record none).
func (g *FakeNMIGateway) SaleCount() int { return len(g.Sales()) }

// SaleRequestCount includes declined submissions as well as recorded sales.
func (g *FakeNMIGateway) SaleRequestCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case r.Form.Get("type") == "sale":
		g.serveSale(w, r)
	case r.Form.Get("report_type") == "transaction":
		g.serveSearch(w, r.Form.Get("order_id"))
	default:
		http.Error(w, "unsupported fake NMI request", http.StatusNotImplemented)
	}
}

func (g *FakeNMIGateway) serveSale(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests++
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
	if p, ok := g.plans[r.Form.Get("subscription_id")]; ok && sale.Amount == "" {
		sale.Amount, sale.Currency = p.amount, p.currency
	}
	g.sales = append(g.sales, sale)
	if hold := g.hold; hold != nil {
		// Mid-request at the provider: unlock so reads can proceed, and wait.
		g.held++
		g.mu.Unlock()
		<-hold
		g.mu.Lock()
		g.held--
	}
	if g.mode == NMISaleUncertain {
		fmt.Fprint(w, "response=3&responsetext=Communication+error&response_code=421")
		return
	}
	fmt.Fprintf(w, "response=1&responsetext=SUCCESS&authcode=123456&transactionid=%s&orderid=%s&response_code=100", sale.TransactionID, sale.OrderID)
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
