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
