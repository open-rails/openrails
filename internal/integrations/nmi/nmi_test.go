package nmi

import (
	"net/http"
	"net/http/httptest"
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

func TestDeleteRecurringSubscriptionBlockedByKillSwitch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request must reach NMI while subscription deletes are disabled")
	}))
	defer server.Close()

	client, err := NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "test-security-key",
	}, false)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	client.SubscriptionDeletesDisabled = true

	err = client.DeleteRecurringSubscription("12345")
	require.ErrorIs(t, err, ErrSubscriptionDeletesDisabled)
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

	// Every NMI mutation funnels through sendDirectRequest; a delete is a
	// representative write.
	err = client.DeleteRecurringSubscription("12345")
	require.ErrorIs(t, err, ErrProviderReadOnly)
}

// probeServer simulates an NMI gateway for the test-mode probe. verdicts maps
// the auth amount to the response code returned ("1" approved, "2" declined,
// "3" error).
func probeServer(t *testing.T, verdicts map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.Form.Get("type") {
		case "auth":
			code, ok := verdicts[r.Form.Get("amount")]
			if !ok {
				t.Errorf("unexpected probe amount %q", r.Form.Get("amount"))
				code = "3"
			}
			w.Write([]byte("response=" + code + "&responsetext=PROBE&transactionid=99001&response_code=100"))
		case "void":
			w.Write([]byte("response=1&responsetext=SUCCESS&transactionid=99001"))
		default:
			t.Errorf("unexpected probe request type %q", r.Form.Get("type"))
		}
	}))
}

func probeClient(t *testing.T, serverURL string) *NMIClient {
	t.Helper()
	client, err := NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test-security-key"}, false)
	require.NoError(t, err)
	client.DirectPostURL = serverURL
	return client
}

func TestProbeTestMode(t *testing.T) {
	t.Run("simulation signature: $1 approved + $0.50 declined -> simulated", func(t *testing.T) {
		server := probeServer(t, map[string]string{"1.00": "1", "0.50": "2"})
		defer server.Close()
		result, err := probeClient(t, server.URL).ProbeTestMode()
		require.NoError(t, err)
		require.Equal(t, ProbeSimulated, result)
	})

	t.Run("$1 declined -> live account", func(t *testing.T) {
		server := probeServer(t, map[string]string{"1.00": "2"})
		defer server.Close()
		result, err := probeClient(t, server.URL).ProbeTestMode()
		require.NoError(t, err)
		require.Equal(t, ProbeLive, result)
	})

	t.Run("approves everything -> not the simulator signature -> live", func(t *testing.T) {
		server := probeServer(t, map[string]string{"1.00": "1", "0.50": "1"})
		defer server.Close()
		result, err := probeClient(t, server.URL).ProbeTestMode()
		require.NoError(t, err)
		require.Equal(t, ProbeLive, result)
	})

	t.Run("gateway error (bad credentials) -> indeterminate", func(t *testing.T) {
		server := probeServer(t, map[string]string{"1.00": "3"})
		defer server.Close()
		result, err := probeClient(t, server.URL).ProbeTestMode()
		require.Error(t, err)
		require.Equal(t, ProbeIndeterminate, result)
	})

	t.Run("transport failure -> indeterminate", func(t *testing.T) {
		server := probeServer(t, nil)
		server.Close() // refuse connections
		result, err := probeClient(t, server.URL).ProbeTestMode()
		require.Error(t, err)
		require.Equal(t, ProbeIndeterminate, result)
	})
}
