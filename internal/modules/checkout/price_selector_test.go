package checkout

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExplicitPriceKeyPreservesAcceptedIDFingerprint(t *testing.T) {
	request := CheckoutSessionCreateRequest{
		PriceID:    "price_11111111-1111-1111-1111-111111111111",
		Payment:    CheckoutSessionPaymentRequest{Rail: "stripe"},
		Metadata:   map[string]string{"post": "post-123"},
		SuccessURL: "https://app.example/success", CancelURL: "https://app.example/cancel",
	}
	// Frozen from the pre-PriceKey request projection. Adding an optional key
	// must not strand an existing durable ID-only checkout after an upgrade.
	const accepted = "5ce91979357cb141b7732e5727ae33b77e1d8892c42f3b0193d6b08002cac345"
	require.Equal(t, accepted, checkoutSessionRequestFingerprint(&request))
	request.PriceKey, request.PriceID = request.PriceID, ""
	require.NotEqual(t, accepted, checkoutSessionRequestFingerprint(&request), "the same spelling in a key field is a different selection")
}
