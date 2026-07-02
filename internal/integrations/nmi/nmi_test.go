package nmi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient_EndpointSelection(t *testing.T) {
	baseCfg := &config.NMIProviderSettings{
		SecurityKey: "test-security-key",
	}

	t.Run("test mode uses fixed endpoints", func(t *testing.T) {
		client, err := NewClient("mobius", baseCfg, true)
		require.NoError(t, err)
		assert.Equal(t, DefaultDirectPostURL, client.DirectPostURL, "should use fixed direct post URL")
		assert.Equal(t, DefaultQueryAPIURL, client.QueryURL, "should use fixed query URL")
		assert.True(t, client.TestMode)
	})

	t.Run("production mode uses default endpoints", func(t *testing.T) {
		client, err := NewClient("mobius", baseCfg, false)
		require.NoError(t, err)
		assert.Equal(t, DefaultDirectPostURL, client.DirectPostURL, "should use production direct post URL")
		assert.Equal(t, DefaultQueryAPIURL, client.QueryURL, "should use production query URL")
		assert.False(t, client.TestMode)
	})

	t.Run("production mode requires security key", func(t *testing.T) {
		emptyCfg := &config.NMIProviderSettings{}
		_, err := NewClient("mobius", emptyCfg, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "security key is required")
	})

	t.Run("test mode allows missing security key", func(t *testing.T) {
		emptyCfg := &config.NMIProviderSettings{}
		client, err := NewClient("mobius", emptyCfg, true)
		require.NoError(t, err)
		// Client created but SecurityKey is empty (API calls will fail but that's expected)
		assert.Empty(t, client.SecurityKey)
	})
}

func TestAttemptManualRebill_SendsStableOrderReferences(t *testing.T) {
	requestSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "rebill-subscription-period", r.Form.Get("orderid"))
		assert.Equal(t, "rebill-subscription-period", r.Form.Get("ponumber"))
		requestSeen <- struct{}{}
		_, _ = w.Write([]byte("response=1&transactionid=txn_rebill_123"))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "test-security-key",
	}, false)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	client.QueryURL = server.URL

	resp, err := client.AttemptManualRebill(ManualRebillParams{
		VaultID:        "vault_123",
		BillingID:      "billing_123",
		SubscriptionID: "sub_123",
		OrderID:        "rebill-subscription-period",
		PONumber:       "rebill-subscription-period",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.True(t, resp.Success)
	require.Equal(t, "txn_rebill_123", resp.TransactionID)
	<-requestSeen
}

func TestReadOnlyBlocksAllDirectPostMutations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no direct-post request may reach NMI while the client is read-only")
	}))
	defer server.Close()

	client, err := NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "test-security-key",
	}, false)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	client.QueryURL = server.URL
	client.ReadOnly = true

	client.V5BaseURL = server.URL

	// A v5 write (delete) and a classic direct-post write (rebill) are the two
	// representative mutation transports; both must be blocked.
	err = client.DeleteRecurringSubscription("12345")
	require.ErrorIs(t, err, ErrProviderReadOnly)
	_, err = client.AttemptManualRebill(ManualRebillParams{VaultID: "v", BillingID: "b", SubscriptionID: "s"})
	require.ErrorIs(t, err, ErrProviderReadOnly)
}

// probeServer simulates the NMI v5 gateway for the test-mode probe. authCode
// is the response returned for auths ("1" approved, "2" declined, "3" error).
// Every auth's randomized amount and order id are validated and recorded into
// seen (when non-nil) for repeat-safety assertions.
func probeServer(t *testing.T, authCode string, seen *[]url.Values) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/payments/auth":
			var req struct {
				Amount         float64 `json:"amount"`
				PaymentDetails struct {
					CardNumber string `json:"card_number"`
					CardExp    string `json:"card_exp"`
				} `json:"payment_details"`
				OrderDetails struct {
					ID string `json:"id"`
				} `json:"order_details"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			// The probe amount must stay in the randomized [$1.01, $1.99]
			// band and the order id must keep the auditable prefix (#362).
			require.GreaterOrEqual(t, req.Amount, 1.01)
			require.LessOrEqual(t, req.Amount, 1.99)
			require.True(t, strings.HasPrefix(req.OrderDetails.ID, "openrails-testmode-probe-"),
				"unexpected probe order id %q", req.OrderDetails.ID)
			require.Equal(t, probeTestCard, req.PaymentDetails.CardNumber)
			if seen != nil {
				form := url.Values{}
				form.Set("order_id", req.OrderDetails.ID)
				form.Set("amount", strconv.FormatFloat(req.Amount, 'f', 2, 64))
				*seen = append(*seen, form)
			}
			_, _ = w.Write([]byte(`{"object":"transaction","id":"99001","response":"` + authCode + `","response_text":"PROBE","response_code":"100"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/payments/99001/void":
			_, _ = w.Write([]byte(`{"object":"transaction","id":"99001","response":"1","response_text":"SUCCESS"}`))
		default:
			t.Errorf("unexpected probe request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func probeClient(t *testing.T, serverURL string) *NMIClient {
	t.Helper()
	client, err := NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test-security-key"}, false)
	require.NoError(t, err)
	client.DirectPostURL = serverURL
	client.V5BaseURL = serverURL
	return client
}

func TestProbeTestMode(t *testing.T) {
	t.Run("test card approved -> simulating (only a simulator approves a non-issued PAN)", func(t *testing.T) {
		server := probeServer(t, "1", nil)
		defer server.Close()
		result, err := probeClient(t, server.URL).ProbeTestMode()
		require.NoError(t, err)
		require.Equal(t, ProbeSimulated, result)
	})

	t.Run("test card declined -> live account", func(t *testing.T) {
		server := probeServer(t, "2", nil)
		defer server.Close()
		result, err := probeClient(t, server.URL).ProbeTestMode()
		require.NoError(t, err)
		require.Equal(t, ProbeLive, result)
	})

	t.Run("gateway error (bad credentials) -> indeterminate", func(t *testing.T) {
		server := probeServer(t, "3", nil)
		defer server.Close()
		result, err := probeClient(t, server.URL).ProbeTestMode()
		require.Error(t, err)
		require.Equal(t, ProbeIndeterminate, result)
	})

	t.Run("transport failure -> indeterminate", func(t *testing.T) {
		server := probeServer(t, "1", nil)
		server.Close() // refuse connections
		result, err := probeClient(t, server.URL).ProbeTestMode()
		require.Error(t, err)
		require.Equal(t, ProbeIndeterminate, result)
	})

	t.Run("repeated probes randomize order_id so duplicate detection never trips (#362)", func(t *testing.T) {
		var seen []url.Values
		server := probeServer(t, "1", &seen)
		defer server.Close()
		client := probeClient(t, server.URL)
		for range 3 {
			result, err := client.ProbeTestMode()
			require.NoError(t, err)
			require.Equal(t, ProbeSimulated, result)
		}
		require.Len(t, seen, 3)
		orderIDs := map[string]bool{}
		for _, form := range seen {
			orderIDs[form.Get("order_id")] = true
		}
		require.Len(t, orderIDs, 3, "each probe must carry a unique order_id")
	})
}

// TestUpdateCustomerVault_ResolvesBillingID: the live gateway requires
// billing[].id on customer PATCH, so the update reads the customer first and
// targets the priority-1 billing entry.
func TestUpdateCustomerVault_ResolvesBillingID(t *testing.T) {
	var patchBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/customers":
			require.Equal(t, "vault-9", r.URL.Query().Get("id"))
			_, _ = w.Write([]byte(`{"customers":[{"object":"customer","id":"vault-9","billing":[{"object":"billing","id":"B77","priority":1}]}],"next_cursor":null,"has_more":false}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/customers/vault-9":
			raw, _ := io.ReadAll(r.Body)
			patchBody = string(raw)
			_, _ = w.Write([]byte(`{"object":"customer","id":"vault-9"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "k"}, false)
	require.NoError(t, err)
	client.V5BaseURL = server.URL

	err = client.UpdateCustomerVault(UpdateCustomerVaultData{
		CustomerVaultID:         "vault-9",
		CreateCustomerVaultData: CreateCustomerVaultData{PaymentToken: "tok_new", FirstName: "Ada"},
	})
	require.NoError(t, err)
	assert.Contains(t, patchBody, `"id":"B77"`)
	assert.Contains(t, patchBody, `"payment_token":"tok_new"`)
	assert.Contains(t, patchBody, `"first_name":"Ada"`)
}
