package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReturnURLAllowedIsExactOrigin(t *testing.T) {
	cfg := &Config{ReturnOrigins: []string{"https://shop.example.com"}}
	for raw, want := range map[string]bool{
		"https://shop.example.com/return?ok=1":          true,
		"https://SHOP.example.com/x":                    true,
		"https://shop.example.com.evil.test/return":     false,
		"https://evil.test/https://shop.example.com":    false,
		"http://shop.example.com/return":                false,
		"https://shop.example.com:8443/return":          false,
		"https://user@shop.example.com/return":          false,
		"//shop.example.com/return":                     false,
		"javascript:alert(1)//https://shop.example.com": false,
		"": false,
	} {
		require.Equal(t, want, cfg.ReturnURLAllowed(raw), raw)
	}
	require.Equal(t, []string{"https://billing.example.com"}, (&Config{PublicBillingBaseURL: "https://billing.example.com/billing"}).AllowedReturnOrigins())
	require.False(t, (&Config{}).ReturnURLAllowed("https://shop.example.com/"))
}

func TestWebhookSecretOverlapBounds(t *testing.T) {
	d, err := (&Config{}).WebhookSecretOverlapDuration()
	require.NoError(t, err)
	require.Equal(t, 24*time.Hour, d)
	for _, bad := range []string{"0s", "-1h", "169h", "soon"} {
		_, err := (&Config{WebhookSecretOverlap: bad}).WebhookSecretOverlapDuration()
		require.Error(t, err, bad)
	}
}
