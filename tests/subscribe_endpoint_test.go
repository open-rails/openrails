//go:build integration

package tests

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
)

// MockNMIServer simulates the NMI Direct Post API for testing
type MockNMIServer struct {
	Server            *httptest.Server
	RequestCount      int32
	LastRequest       map[string][]string
	ResponseOverride  string
	ShouldFail        bool
	FailReason        string
	IDPrefix          string
	VaultIDCounter    int32
	SubscriptionIDGen int32
	DeletedSubs       map[string]bool
	deletedSubsMu     sync.Mutex
}

// NewMockNMIServer creates a new mock NMI server
func NewMockNMIServer() *MockNMIServer {
	mock := &MockNMIServer{IDPrefix: uuid.NewString()[:8]}
	mock.Server = httptest.NewServer(http.HandlerFunc(mock.handleRequest))
	return mock
}

func (m *MockNMIServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&m.RequestCount, 1)

	// v5 JSON surface (path-routed): vault CRUD, sale/auth/refund/void,
	// subscription GET/DELETE. Classic direct-post/query handling below keeps
	// serving the deliberate classic survivors (add_subscription, rebill,
	// update_subscription, edit_plan, transaction search).
	if r.URL.Path != "/" && r.URL.Path != "" {
		m.handleV5(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	m.LastRequest = r.Form

	// Determine what type of request this is
	customerVault := r.Form.Get("customer_vault")
	recurring := r.Form.Get("recurring")

	var response string

	if m.ResponseOverride != "" {
		response = m.ResponseOverride
	} else if m.ShouldFail {
		failReason := m.FailReason
		if failReason == "" {
			failReason = "DECLINE"
		}
		response = fmt.Sprintf("response=2&responsetext=%s&response_code=300", failReason)
	} else if r.Form.Get("report_type") == "profile" {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<response><merchant><company>OpenRails Test Merchant</company><email>billing@test.example</email></merchant></response>`)
		return
	} else if customerVault == "add_customer" {
		// Create customer vault response
		railCustomerRef := fmt.Sprintf("vault_%s_%d", m.IDPrefix, atomic.AddInt32(&m.VaultIDCounter, 1))
		response = fmt.Sprintf("response=1&responsetext=SUCCESS&customer_vault_id=%s", railCustomerRef)
	} else if customerVault == "update_customer" {
		response = "response=1&responsetext=SUCCESS"
	} else if customerVault == "delete_customer" {
		response = "response=1&responsetext=SUCCESS"
	} else if recurring == "add_subscription" {
		// Add subscription response
		subID := fmt.Sprintf("sub_%s_%d", m.IDPrefix, atomic.AddInt32(&m.SubscriptionIDGen, 1))
		txnID := fmt.Sprintf("txn_%s_%d", m.IDPrefix, atomic.AddInt32(&m.SubscriptionIDGen, 1))
		response = fmt.Sprintf("response=1&responsetext=SUCCESS&subscription_id=%s&transactionid=%s&authcode=123456&type=sale", subID, txnID)
	} else if recurring == "delete_subscription" {
		response = "response=1&responsetext=SUCCESS"
	} else if recurring == "update_subscription" {
		// Update subscription response (used for payment method changes)
		response = "response=1&responsetext=SUCCESS"
	} else if recurring == "rebill_subscription" {
		txnID := fmt.Sprintf("txn_%s_%d", m.IDPrefix, atomic.AddInt32(&m.SubscriptionIDGen, 1))
		response = fmt.Sprintf("response=1&responsetext=SUCCESS&transactionid=%s", txnID)
	} else {
		// Default sale response
		txnID := fmt.Sprintf("txn_%s_%d", m.IDPrefix, atomic.AddInt32(&m.SubscriptionIDGen, 1))
		response = fmt.Sprintf("response=1&responsetext=SUCCESS&transactionid=%s&authcode=123456&type=sale", txnID)
	}

	w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(response))
}

// handleV5 answers the v5 JSON routes the client now uses. Failure knobs
// (ShouldFail/FailReason) map onto declined transactions exactly like the
// classic branch so existing tests keep their semantics.
func (m *MockNMIServer) handleV5(w http.ResponseWriter, r *http.Request) {
	m.LastRequest = map[string][]string{"v5_path": {r.Method + " " + r.URL.Path}}
	w.Header().Set("Content-Type", "application/json")

	txnJSON := func(txnID string) string {
		if m.ShouldFail {
			failReason := m.FailReason
			if failReason == "" {
				failReason = "DECLINE"
			}
			return fmt.Sprintf(`{"object":"transaction","id":"%s","response":"2","response_text":"%s","response_code":"300"}`, txnID, failReason)
		}
		return fmt.Sprintf(`{"object":"transaction","id":"%s","response":"1","response_text":"SUCCESS","response_code":"100","auth_code":"123456"}`, txnID)
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/customers":
		if m.ShouldFail {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"type":"validationError","error_code":"E_VALIDATION","message":"%s"}`, m.FailReason)
			return
		}
		railCustomerRef := fmt.Sprintf("vault_%s_%d", m.IDPrefix, atomic.AddInt32(&m.VaultIDCounter, 1))
		fmt.Fprintf(w, `{"object":"customer","id":"%s","billing":[{"object":"billing","id":"B1","priority":1}]}`, railCustomerRef)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/customers/"):
		_, _ = w.Write([]byte(`{"object":"customer","id":"` + strings.TrimPrefix(r.URL.Path, "/customers/") + `"}`))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/customers/"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && (r.URL.Path == "/payments/sale" || r.URL.Path == "/payments/auth"):
		txnID := fmt.Sprintf("txn_%s_%d", m.IDPrefix, atomic.AddInt32(&m.SubscriptionIDGen, 1))
		_, _ = w.Write([]byte(txnJSON(txnID)))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/refund"),
		r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/void"):
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		_, _ = w.Write([]byte(txnJSON(parts[1])))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/payments/"):
		id := strings.TrimPrefix(r.URL.Path, "/payments/")
		fmt.Fprintf(w, `{"object":"transaction","id":"%s","response":"1","actions":[{"id":"%s","type":"sale","success":true,"amount":"1.00"}]}`, id, id)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/subscriptions/"):
		id := strings.TrimPrefix(r.URL.Path, "/subscriptions/")
		if m.deletedSub(id) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"subscription not found"}`))
			return
		}
		fmt.Fprintf(w, `{"object":"subscription","id":"%s","delayed_condition":"active","next_billing_date":"%s"}`, id, time.Now().UTC().Add(30*24*time.Hour).Format("2006-01-02"))
	case r.Method == http.MethodGet && r.URL.Path == "/subscriptions":
		_, _ = w.Write([]byte(`{"subscriptions":[],"next_cursor":null,"has_more":false}`))
	case r.Method == http.MethodGet && r.URL.Path == "/customers":
		// id-filtered lookup (UpdateCustomerVault resolves the priority-1
		// billing id with a read first): echo the customer with one billing.
		if id := r.URL.Query().Get("id"); id != "" {
			fmt.Fprintf(w, `{"customers":[{"object":"customer","id":"%s","billing":[{"object":"billing","id":"B1","priority":1}]}],"next_cursor":null,"has_more":false}`, id)
			return
		}
		_, _ = w.Write([]byte(`{"customers":[],"next_cursor":null,"has_more":false}`))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/subscriptions/"):
		m.markSubDeleted(strings.TrimPrefix(r.URL.Path, "/subscriptions/"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/plans/"):
		id := strings.TrimPrefix(r.URL.Path, "/plans/")
		fmt.Fprintf(w, `{"object":"plan","id":"%s","plan_name":"Mock Plan","plan_amount":"9.99","day_frequency":"30"}`, id)
	case r.Method == http.MethodGet && r.URL.Path == "/plans":
		_, _ = w.Write([]byte(`{"plans":[],"next_cursor":null,"has_more":false}`))
	case r.Method == http.MethodPost && r.URL.Path == "/plans":
		_, _ = w.Write([]byte(`{"object":"plan","id":"mock-plan"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_ROUTE_NOT_FOUND","message":"Route not found"}`))
	}
}

func (m *MockNMIServer) markSubDeleted(id string) {
	m.deletedSubsMu.Lock()
	defer m.deletedSubsMu.Unlock()
	if m.DeletedSubs == nil {
		m.DeletedSubs = map[string]bool{}
	}
	m.DeletedSubs[id] = true
}

func (m *MockNMIServer) deletedSub(id string) bool {
	m.deletedSubsMu.Lock()
	defer m.deletedSubsMu.Unlock()
	return m.DeletedSubs[id]
}

func (m *MockNMIServer) Close() {
	m.Server.Close()
}

func (m *MockNMIServer) URL() string {
	return m.Server.URL
}

func (m *MockNMIServer) Reset() {
	atomic.StoreInt32(&m.RequestCount, 0)
	m.LastRequest = nil
	m.ResponseOverride = ""
	m.ShouldFail = false
	m.FailReason = ""
}

// SetupSuiteWithMockNMI creates a test suite with mock NMI client configured
func SetupSuiteWithMockNMI(t *testing.T) (*TestContainerSuite, *MockNMIServer) {
	suite := setupTestSuite(t, WithSuiteClock(clockwork.NewRealClock()))
	mock := NewMockNMIServer()

	// #788: every NMI consumer arms per charge from the armed rail state (the
	// suite-seeded mobius account); only the gateway endpoint is overridden.
	suite.SetNMIGateway(mock.URL())

	t.Cleanup(func() {
		suite.SetNMIGateway("")
		mock.Close()
	})

	return suite, mock
}
