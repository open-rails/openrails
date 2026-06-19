package nmi

import (
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

	// Every NMI mutation funnels through sendDirectRequest; a delete is a
	// representative write.
	err = client.DeleteRecurringSubscription("12345")
	require.ErrorIs(t, err, ErrProviderReadOnly)
}

// probeServer simulates an NMI gateway for the test-mode probe. authCode is
// the response code returned for auths ("1" approved, "2" declined, "3"
// error). Every auth's randomized amount and order_id are validated and
// recorded into seen (when non-nil) for repeat-safety assertions.
func probeServer(t *testing.T, authCode string, seen *[]url.Values) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.Form.Get("type") {
		case "auth":
			// The probe amount must stay in the randomized [$1.01, $1.99]
			// band and the order_id must keep the auditable prefix (#362).
			amount, err := strconv.ParseFloat(r.Form.Get("amount"), 64)
			require.NoError(t, err)
			require.GreaterOrEqual(t, amount, 1.01)
			require.LessOrEqual(t, amount, 1.99)
			require.True(t, strings.HasPrefix(r.Form.Get("order_id"), "openrails-testmode-probe-"),
				"unexpected probe order_id %q", r.Form.Get("order_id"))
			if seen != nil {
				*seen = append(*seen, r.Form)
			}
			w.Write([]byte("response=" + authCode + "&responsetext=PROBE&transactionid=99001&response_code=100"))
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
