//go:build integration

package tests

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
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
}

// NewMockNMIServer creates a new mock NMI server
func NewMockNMIServer() *MockNMIServer {
	mock := &MockNMIServer{IDPrefix: uuid.NewString()[:8]}
	mock.Server = httptest.NewServer(http.HandlerFunc(mock.handleRequest))
	return mock
}

func (m *MockNMIServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&m.RequestCount, 1)

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
	} else if customerVault == "add_customer" {
		// Create customer vault response
		vaultID := fmt.Sprintf("vault_%s_%d", m.IDPrefix, atomic.AddInt32(&m.VaultIDCounter, 1))
		response = fmt.Sprintf("response=1&responsetext=SUCCESS&customer_vault_id=%s", vaultID)
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

	// Create NMI client with mock server URL
	nmiSettings := &config.NMIProviderSettings{
		Name:        "mobius",
		SecurityKey: "test-security-key",
		TestMode:    true,
	}

	client, err := nmi.NewClient("mobius", nmiSettings, true) // true = test mode (sandbox endpoints)
	require.NoError(t, err)

	// Override the DirectPostURL to point to mock server
	client.DirectPostURL = mock.URL()

	// Inject the mock client into the runtime
	suite.App.Runtime.NMIClients = map[string]*nmi.NMIClient{
		"mobius": client,
	}

	// Also update the subscription service's NMI clients
	if suite.App.Runtime.SubscriptionService != nil {
		suite.App.Runtime.SubscriptionService.NMIClients = suite.App.Runtime.NMIClients
	}
	if suite.App.Runtime.VaultService != nil {
		suite.App.Runtime.VaultService.NMIClients = suite.App.Runtime.NMIClients
	}
	if suite.App.Runtime.CheckoutService != nil {
		suite.App.Runtime.CheckoutService.NMIClients = suite.App.Runtime.NMIClients
		if suite.App.Runtime.CheckoutService.NMISaleService != nil {
			suite.App.Runtime.CheckoutService.NMISaleService.NMIClients = suite.App.Runtime.NMIClients
		}
	}

	t.Cleanup(func() {
		mock.Close()
	})

	return suite, mock
}
