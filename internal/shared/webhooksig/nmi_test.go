package webhooksig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyNMISignature(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"event_id":"evt_123","event_type":"transaction.sale.success"}`)

	t.Run("accepts valid signature", func(t *testing.T) {
		ts := "1700000000"
		header := fmt.Sprintf("t=%s,s=%s", ts, signNMIHeader(secret, ts, body))
		require.NoError(t, VerifyNMISignature(secret, header, body))
	})

	t.Run("accepts opaque nonce format", func(t *testing.T) {
		ts := "nonce-value-not-a-timestamp"
		header := fmt.Sprintf("s=%s,t=%s", signNMIHeader(secret, ts, body), ts)
		require.NoError(t, VerifyNMISignature(secret, header, body))
	})

	t.Run("rejects malformed signature header", func(t *testing.T) {
		err := VerifyNMISignature(secret, "invalid-header", body)
		require.Error(t, err)
		require.ErrorContains(t, err, "unrecognized webhook signature format")
	})

	t.Run("rejects invalid signature", func(t *testing.T) {
		err := VerifyNMISignature(secret, "t=1700000000,s=invalid-signature", body)
		require.Error(t, err)
		require.ErrorContains(t, err, "invalid webhook signature")
	})
}

func TestValidateNMISignature(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"event_id":"evt_123"}`)

	t.Run("uses php signature when present", func(t *testing.T) {
		ts := "1700000000"
		phpSig := fmt.Sprintf("t=%s,s=%s", ts, signNMIHeader(secret, ts, body))
		legacyCalled := false

		sig, err := ValidateNMISignature(secret, body, phpSig, []string{"legacy"}, func(signature string) error {
			legacyCalled = true
			return nil
		})

		require.NoError(t, err)
		require.Equal(t, phpSig, sig)
		require.False(t, legacyCalled)
	})

	t.Run("does not fallback on invalid php signature", func(t *testing.T) {
		legacyCalled := false
		_, err := ValidateNMISignature(secret, body, "t=1700000000,s=invalid", []string{"legacy"}, func(signature string) error {
			legacyCalled = true
			return nil
		})

		require.Error(t, err)
		require.ErrorIs(t, err, ErrNMIWebhookSignatureInvalid)
		require.False(t, legacyCalled)
	})

	t.Run("uses legacy signature when php header missing", func(t *testing.T) {
		legacyCalled := false
		sig, err := ValidateNMISignature(secret, body, "", []string{"", "legacy"}, func(signature string) error {
			legacyCalled = true
			require.Equal(t, "legacy", signature)
			return nil
		})

		require.NoError(t, err)
		require.Equal(t, "legacy", sig)
		require.True(t, legacyCalled)
	})

	t.Run("returns missing secret", func(t *testing.T) {
		_, err := ValidateNMISignature("", body, "", []string{"legacy"}, func(signature string) error { return nil })
		require.ErrorIs(t, err, ErrNMIWebhookSecretMissing)
	})

	t.Run("returns missing signature", func(t *testing.T) {
		_, err := ValidateNMISignature(secret, body, "", nil, func(signature string) error { return nil })
		require.ErrorIs(t, err, ErrNMIWebhookSignatureMissing)
	})
}

func signNMIHeader(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "." + string(body)))
	return hex.EncodeToString(mac.Sum(nil))
}
