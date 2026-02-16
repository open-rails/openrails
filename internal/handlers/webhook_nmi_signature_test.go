package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyNMIWebhookSignature(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"event_id":"evt_123","event_type":"transaction.sale.success"}`)

	t.Run("accepts valid signature", func(t *testing.T) {
		ts := "1700000000"
		header := fmt.Sprintf("t=%s,s=%s", ts, signNMIHeader(secret, ts, body))

		err := verifyNMIWebhookSignature(secret, header, body)
		require.NoError(t, err)
	})

	t.Run("accepts opaque nonce format", func(t *testing.T) {
		ts := "nonce-value-not-a-timestamp"
		header := fmt.Sprintf("s=%s,t=%s", signNMIHeader(secret, ts, body), ts)

		err := verifyNMIWebhookSignature(secret, header, body)
		require.NoError(t, err)
	})

	t.Run("rejects malformed signature header", func(t *testing.T) {
		err := verifyNMIWebhookSignature(secret, "invalid-header", body)
		require.Error(t, err)
		require.ErrorContains(t, err, "unrecognized webhook signature format")
	})

	t.Run("rejects invalid signature", func(t *testing.T) {
		ts := "1700000000"
		header := fmt.Sprintf("t=%s,s=%s", ts, "invalid-signature")

		err := verifyNMIWebhookSignature(secret, header, body)
		require.Error(t, err)
		require.ErrorContains(t, err, "invalid webhook signature")
	})
}

func signNMIHeader(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "." + string(body)))
	return hex.EncodeToString(mac.Sum(nil))
}
