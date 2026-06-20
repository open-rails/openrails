//go:build integration

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// stripeSig builds a Stripe-style signature header for the merchant-webhook
// integration test. The per-merchant webhook trust boundary (cross-tenant secret
// isolation, missing/invalid signature) is unit-tested in the handlers package
// against prepareStripeMultiSecret — the function the live MerchantWebhook handler
// uses — and asserted end-to-end (wrong secret → 401) by the integration test.
func stripeSig(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}
