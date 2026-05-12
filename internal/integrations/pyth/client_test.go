package pyth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testFeedID = "eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a"

func TestClientPriceUSD(t *testing.T) {
	now := time.Now().UTC()
	server := newPythTestServer(t, fmt.Sprintf(`{
		"parsed": [{
			"id": %q,
			"price": {"price": "100001000", "conf": "1000", "expo": -8, "publish_time": %d}
		}]
	}`, testFeedID, now.Unix()))

	client := newTestClient(t, server.URL)
	client.now = func() time.Time { return now }

	price, err := client.PriceUSD(context.Background(), "usdc")
	require.NoError(t, err)
	require.Equal(t, 1.00001, price)
}

func TestClientRejectsStalePrice(t *testing.T) {
	now := time.Now().UTC()
	server := newPythTestServer(t, fmt.Sprintf(`{
		"parsed": [{
			"id": %q,
			"price": {"price": "100000000", "conf": "1000", "expo": -8, "publish_time": %d}
		}]
	}`, testFeedID, now.Add(-10*time.Minute).Unix()))

	client := newTestClient(t, server.URL)
	client.now = func() time.Time { return now }

	_, err := client.PriceUSD(context.Background(), "USDC")
	require.ErrorContains(t, err, "stale")
}

func TestClientRejectsHighConfidenceBPS(t *testing.T) {
	now := time.Now().UTC()
	server := newPythTestServer(t, fmt.Sprintf(`{
		"parsed": [{
			"id": %q,
			"price": {"price": "100000000", "conf": "2000000", "expo": -8, "publish_time": %d}
		}]
	}`, testFeedID, now.Unix()))

	client := newTestClient(t, server.URL)
	client.now = func() time.Time { return now }

	_, err := client.PriceUSD(context.Background(), "USDC")
	require.ErrorContains(t, err, "confidence")
}

func TestClientRequiresConfiguredFeed(t *testing.T) {
	client := newTestClient(t, "https://example.com")

	_, err := client.PriceUSD(context.Background(), "PYUSD")
	require.ErrorContains(t, err, "price feed missing")
}

func newTestClient(t *testing.T, hermesURL string) *Client {
	t.Helper()
	client, err := NewClient(Config{
		HermesURL:        hermesURL,
		MaxPriceAge:      2 * time.Minute,
		MaxConfidenceBPS: 100,
		PriceFeeds: map[string]string{
			"USDC": testFeedID,
		},
	})
	require.NoError(t, err)
	return client
}

func newPythTestServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/updates/price/latest", r.URL.Path)
		require.True(t, strings.Contains(r.URL.RawQuery, "ids"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(response))
		require.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	return server
}
