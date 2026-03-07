package webhookutil

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCanonicalProvider(t *testing.T) {
	require.Equal(t, "mobius", CanonicalProvider(" Mobius "))
	require.Equal(t, "stripe", CanonicalProvider("/stripe/"))
}

func TestParseStripeEventMeta(t *testing.T) {
	eventID, eventType, err := ParseStripeEventMeta([]byte(`{"id":"evt_123","type":"invoice.paid"}`))
	require.NoError(t, err)
	require.Equal(t, "evt_123", eventID)
	require.Equal(t, "invoice.paid", eventType)
}

func TestComputeUniqueKey(t *testing.T) {
	require.Equal(t, "webhook:mobius:evt_123", ComputeUniqueKey("mobius", "evt_123", "", []byte(`{}`)))
	require.NotEmpty(t, ComputeUniqueKey("ccbill", "", "NewSaleSuccess", []byte(`{"a":1}`)))
}

func TestNormalizeCCBillPayload(t *testing.T) {
	normalized, err := NormalizeCCBillPayload([]byte("eventType=NewSaleSuccess&subscriptionId=123"))
	require.NoError(t, err)
	require.JSONEq(t, `{"eventType":"NewSaleSuccess","subscriptionId":"123"}`, string(normalized))

	normalized, err = NormalizeCCBillPayload([]byte(`{"ok":true}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, string(normalized))
}

func TestVerifyStripeSignature(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"id":"evt_123","type":"checkout.session.completed"}`)
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "." + string(body)))
	signature := hex.EncodeToString(mac.Sum(nil))
	header := fmt.Sprintf("t=%s,v1=%s", timestamp, signature)

	require.NoError(t, VerifyStripeSignature(secret, header, body, time.Minute))

	err := VerifyStripeSignature(secret, fmt.Sprintf("t=%s,v1=bad", timestamp), body, time.Minute)
	require.Error(t, err)
	require.ErrorContains(t, err, "stripe signature mismatch")
}

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

		sig, err := ValidateNMISignature(secret, body, phpSig)

		require.NoError(t, err)
		require.Equal(t, phpSig, sig)
	})

	t.Run("rejects invalid php signature", func(t *testing.T) {
		_, err := ValidateNMISignature(secret, body, "t=1700000000,s=invalid")

		require.Error(t, err)
		require.ErrorIs(t, err, ErrNMIWebhookSignatureInvalid)
	})

	t.Run("returns missing secret", func(t *testing.T) {
		_, err := ValidateNMISignature("", body, "")
		require.ErrorIs(t, err, ErrNMIWebhookSecretMissing)
	})

	t.Run("returns missing signature", func(t *testing.T) {
		_, err := ValidateNMISignature(secret, body, "")
		require.ErrorIs(t, err, ErrNMIWebhookSignatureMissing)
	})
}

func signNMIHeader(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "." + string(body)))
	return hex.EncodeToString(mac.Sum(nil))
}
