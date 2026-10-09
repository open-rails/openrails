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
		require.Equal(t, want, ReturnURLAllowed(cfg, raw), raw)
	}
	require.Equal(t, []string{"https://billing.example.com"}, AllowedReturnOrigins(&Config{PublicBillingBaseURL: "https://billing.example.com/billing"}))
	require.False(t, ReturnURLAllowed(&Config{}, "https://shop.example.com/"))
}

func TestWebhookSecretOverlapBounds(t *testing.T) {
	d, err := WebhookSecretOverlapDuration(&Config{})
	require.NoError(t, err)
	require.Equal(t, 24*time.Hour, d)
	for _, bad := range []string{"0s", "-1h", "169h", "soon"} {
		_, err := WebhookSecretOverlapDuration(&Config{WebhookSecretOverlap: bad})
		require.Error(t, err, bad)
	}
}

func TestCheckoutConfig(t *testing.T) {
	for raw, ok := range map[string]bool{
		"": true, "https://pay.example/checkout": true, "http://localhost:4790/pay.html": true, "http://127.0.0.1:1/p": true,
		"http://pay.example/checkout": false, "https://user@pay.example/": false, "https://pay.example/checkout#x": false, "/checkout": false,
	} {
		require.Equal(t, ok, ValidateCheckout(CheckoutConfig{PageURL: raw}) == nil, raw)
	}
	for raw, ok := range map[string]bool{
		"https://host-one.example": true, "https://host-one.example:8443/": true, "http://localhost:4173": true,
		"https://host-one.example/app": false, "http://host-one.example": false, "host-one.example": false, "https://host-one.example?x=1": false,
	} {
		require.Equal(t, ok, ValidateCheckout(CheckoutConfig{EmbedOrigins: []string{raw}}) == nil, raw)
	}

	cfg := &Config{Checkout: CheckoutConfig{PageURL: "https://pay.example/checkout", EmbedOrigins: []string{"https://Host-One.example/", "https://host-two.example"}}}
	require.Equal(t, "frame-ancestors 'self' https://host-one.example https://host-two.example", CheckoutFrameAncestors(cfg))
	require.True(t, CheckoutEmbedAllowed(cfg, "https://host-two.example"))
	require.True(t, CheckoutEmbedAllowed(cfg, "https://pay.example"), "the payment host may frame its own page")
	require.False(t, CheckoutEmbedAllowed(cfg, "https://host-three.example"))
	require.False(t, CheckoutEmbedAllowed(cfg, ""))
	// The sites framing the page are where its redirect rails return to.
	require.True(t, ReturnURLAllowed(cfg, "https://host-two.example/subscribe"))
	require.False(t, ReturnURLAllowed(cfg, "https://host-three.example/subscribe"))

	for _, single := range []*Config{nil, {}, {Checkout: CheckoutConfig{}}} {
		require.Equal(t, "frame-ancestors 'self'", CheckoutFrameAncestors(single))
		require.False(t, CheckoutEmbedAllowed(single, "https://host-one.example"))
	}
}
