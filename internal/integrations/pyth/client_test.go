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

const feedID = "eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a"

func TestNewClientRequiresHermesAgeAndFeeds(t *testing.T) {
	ok := Config{HermesURL: "https://hermes.example", MaxPriceAge: time.Minute, PriceFeeds: map[string]string{"usdc": feedID}}
	for name, mut := range map[string]func(*Config){
		"no url":   func(c *Config) { c.HermesURL = " " },
		"bad url":  func(c *Config) { c.HermesURL = "not a url" },
		"no age":   func(c *Config) { c.MaxPriceAge = 0 },
		"no feeds": func(c *Config) { c.PriceFeeds = map[string]string{" ": feedID, "USDC": " "} },
	} {
		cfg := ok
		mut(&cfg)
		_, err := NewClient(cfg)
		require.Error(t, err, name)
	}
	_, err := NewClient(ok)
	require.NoError(t, err)
}

// A crypto pricing decision only accepts a fresh, tight, positive price for the
// configured feed; everything else is refused, never defaulted.
func TestLatestPriceValidation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	body := func(id, price, conf string, published time.Time) string {
		return fmt.Sprintf(`{"parsed":[{"id":%q,"price":{"price":%q,"conf":%q,"expo":-8,"publish_time":%d}}]}`, id, price, conf, published.Unix())
	}
	for _, tc := range []struct {
		name, symbol, response string
		status                 int
		want                   float64
		wantErr                string
	}{
		{"valid", " usdc ", body(feedID, "100001000", "1000", now), 200, 1.00001, ""},
		{"feed id case-insensitive", "USDC", body(strings.ToUpper(feedID), "100000000", "0", now), 200, 1, ""},
		{"at max age", "USDC", body(feedID, "100000000", "0", now.Add(-2*time.Minute)), 200, 1, ""},
		{"stale", "USDC", body(feedID, "100000000", "1000", now.Add(-10*time.Minute)), 200, 0, "stale"},
		{"wide confidence", "USDC", body(feedID, "100000000", "2000000", now), 200, 0, "confidence"},
		{"non-positive", "USDC", body(feedID, "0", "0", now), 200, 0, "not positive"},
		{"unparseable", "USDC", body(feedID, "1e", "0", now), 200, 0, "parse"},
		{"no publish time", "USDC", body(feedID, "100000000", "0", time.Unix(0, 0)), 200, 0, "publish time"},
		{"other feed only", "USDC", body("ff", "100000000", "0", now), 200, 0, "unavailable"},
		{"upstream error", "USDC", "", 503, 0, "status"},
		{"unconfigured symbol", "PYUSD", "", 200, 0, "price feed missing"},
		{"blank symbol", " ", "", 200, 0, "required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/v2/updates/price/latest", r.URL.Path)
				require.Equal(t, []string{feedID}, r.URL.Query()["ids[]"])
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer srv.Close()
			c, err := NewClient(Config{HermesURL: srv.URL + "/", MaxPriceAge: 2 * time.Minute, MaxConfidenceBPS: 100, PriceFeeds: map[string]string{"usdc": feedID}})
			require.NoError(t, err)
			c.now = func() time.Time { return now }
			got, err := c.PriceUSD(context.Background(), tc.symbol)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.InDelta(t, tc.want, got, 1e-12)
		})
	}
}
