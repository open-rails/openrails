package webhookutil

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestCanonicalRail(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"nmi", "nmi"},
		{" NMI ", "nmi"},
		{"/stripe/", "stripe"},
		{"ccbill", "ccbill"},
		{"basistheory", "basis_theory"},
	} {
		got, err := CanonicalRail(tc.in)
		require.NoError(t, err, tc.in)
		require.Equal(t, tc.want, got, tc.in)
	}
}

// or#893 phase 6: the webhook path's segment is the gateway KIND. A PSP key is
// not a rail — /webhooks/mobius folded onto nmi, making two URLs mean one
// endpoint and implying a PSP was routable by name. It is refused, with the
// rename.
func TestCanonicalRailRefusesRetiredSegments(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{
			"mobius",
			"retired webhook rail segment: /webhooks/mobius was removed (or#893) — post to /webhooks/nmi; a PSP is named by the account_id path segment or the payload's account identity, not by the rail segment",
		},
		{
			" MOBIUS/",
			"retired webhook rail segment: /webhooks/mobius was removed (or#893) — post to /webhooks/nmi; a PSP is named by the account_id path segment or the payload's account identity, not by the rail segment",
		},
		{
			"basis_theory",
			"retired webhook rail segment: /webhooks/basis_theory was removed (or#893) — post to /webhooks/basistheory; a PSP is named by the account_id path segment or the payload's account identity, not by the rail segment",
		},
	} {
		got, err := CanonicalRail(tc.in)
		require.Empty(t, got, tc.in)
		require.ErrorIs(t, err, ErrWebhookRailRetired, tc.in)
		require.EqualError(t, err, tc.want, tc.in)
	}
}

func TestParseStripeEventMeta(t *testing.T) {
	eventID, eventType, err := ParseStripeEventMeta([]byte(`{"id":"evt_123","type":"invoice.paid"}`))
	require.NoError(t, err)
	require.Equal(t, "evt_123", eventID)
	require.Equal(t, "invoice.paid", eventType)
}

func TestComputeUniqueKey(t *testing.T) {
	require.Equal(t, "webhook:nmi:evt_123", ComputeUniqueKey("nmi", "evt_123", "", []byte(`{}`)))
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
		ts := fmt.Sprintf("%d", time.Now().Unix())
		header := fmt.Sprintf("t=%s,s=%s", ts, signNMIHeader(secret, ts, body))
		require.NoError(t, VerifyNMISignature(secret, header, body))
	})

	t.Run("rejects opaque nonce format", func(t *testing.T) {
		ts := "nonce-value-not-a-timestamp"
		header := fmt.Sprintf("s=%s,t=%s", signNMIHeader(secret, ts, body), ts)
		err := VerifyNMISignature(secret, header, body)
		require.Error(t, err)
		require.ErrorContains(t, err, "invalid webhook signature timestamp")
	})

	t.Run("rejects stale timestamp", func(t *testing.T) {
		ts := "1700000000"
		header := fmt.Sprintf("s=%s,t=%s", signNMIHeader(secret, ts, body), ts)
		err := VerifyNMISignature(secret, header, body)
		require.Error(t, err)
		require.ErrorContains(t, err, "timestamp outside tolerance")
	})

	t.Run("rejects malformed signature header", func(t *testing.T) {
		err := VerifyNMISignature(secret, "invalid-header", body)
		require.Error(t, err)
		require.ErrorContains(t, err, "unrecognized webhook signature format")
	})

	t.Run("rejects invalid signature", func(t *testing.T) {
		ts := fmt.Sprintf("%d", time.Now().Unix())
		err := VerifyNMISignature(secret, fmt.Sprintf("t=%s,s=invalid-signature", ts), body)
		require.Error(t, err)
		require.ErrorContains(t, err, "invalid webhook signature")
	})
}

func TestValidateNMISignature(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"event_id":"evt_123"}`)

	t.Run("uses php signature when present", func(t *testing.T) {
		ts := fmt.Sprintf("%d", time.Now().Unix())
		phpSig := fmt.Sprintf("t=%s,s=%s", ts, signNMIHeader(secret, ts, body))

		sig, err := ValidateNMISignature(secret, body, phpSig)

		require.NoError(t, err)
		require.Equal(t, phpSig, sig)
	})

	t.Run("accepts uppercase quoted signature", func(t *testing.T) {
		ts := fmt.Sprintf("%d", time.Now().Unix())
		phpSig := fmt.Sprintf(`t="%s",s="%s"`, ts, strings.ToUpper(signNMIHeader(secret, ts, body)))

		sig, err := ValidateNMISignature(secret, body, phpSig)

		require.NoError(t, err)
		require.Equal(t, phpSig, sig)
	})

	t.Run("rejects invalid php signature", func(t *testing.T) {
		ts := fmt.Sprintf("%d", time.Now().Unix())
		_, err := ValidateNMISignature(secret, body, fmt.Sprintf("t=%s,s=invalid", ts))

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

func TestPrepareCCBill(t *testing.T) {
	prepared, err := PrepareCCBill([]byte("eventType=NewSaleSuccess&subscriptionId=123"), " NewSaleSuccess ")
	require.NoError(t, err)
	require.Equal(t, subscriptions.RailCCBill, prepared.Rail)
	require.Equal(t, "NewSaleSuccess", prepared.EventType)
	require.JSONEq(t, `{"eventType":"NewSaleSuccess","subscriptionId":"123"}`, string(prepared.Body))

	_, err = PrepareCCBill([]byte("bad=%zz"), "NewSaleSuccess")
	require.ErrorIs(t, err, ErrWebhookPayloadInvalid)

	_, err = PrepareCCBill([]byte(`{"ok":true}`), " ")
	require.ErrorIs(t, err, ErrWebhookEventTypeMissing)

	_, err = PrepareCCBill([]byte(`{"eventType":"Cancellation","subscriptionId":"123"}`), "RenewalSuccess")
	require.ErrorIs(t, err, ErrWebhookEventTypeMismatch)
}

func TestPrepareStripe(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"id":"evt_123","type":"checkout.session.completed"}`)
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	header := fmt.Sprintf("t=%s,v1=%s", timestamp, signNMIHeader(secret, timestamp, body))

	prepared, err := PrepareStripe(body, secret, header, time.Minute)
	require.NoError(t, err)
	require.Equal(t, subscriptions.RailStripe, prepared.Rail)
	require.Equal(t, "evt_123", prepared.EventID)
	require.Equal(t, "checkout.session.completed", prepared.EventType)
	require.True(t, prepared.SignatureVerified)
	require.Equal(t, header, prepared.Signature)

	_, err = PrepareStripe(body, "", header, time.Minute)
	require.ErrorIs(t, err, ErrWebhookSignatureRequired)

	_, err = PrepareStripe(body, secret, "", time.Minute)
	require.ErrorIs(t, err, ErrWebhookSignatureMissing)

	_, err = PrepareStripe(body, secret, fmt.Sprintf("t=%s,v1=bad", timestamp), time.Minute)
	require.ErrorIs(t, err, ErrWebhookSignatureInvalid)

	invalidStripeBody := []byte(`{"id":""}`)
	invalidStripeHeader := fmt.Sprintf("t=%s,v1=%s", timestamp, signNMIHeader(secret, timestamp, invalidStripeBody))
	_, err = PrepareStripe(invalidStripeBody, secret, invalidStripeHeader, time.Minute)
	require.ErrorIs(t, err, ErrWebhookPayloadInvalid)
}

func TestPrepareNMI(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"event_id":"evt_123","event_type":"transaction.sale.success"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	header := fmt.Sprintf("t=%s,s=%s", ts, signNMIHeader(secret, ts, body))

	prepared, err := PrepareNMI(" NMI ", body, secret, header)
	require.NoError(t, err)
	require.Equal(t, "nmi", prepared.Rail)
	require.Equal(t, "evt_123", prepared.EventID)
	require.Equal(t, "transaction.sale.success", prepared.EventType)
	require.True(t, prepared.SignatureVerified)
	require.Equal(t, header, prepared.Signature)

	missingEventIDBody := []byte(`{"event_id":""}`)
	missingEventIDHeader := fmt.Sprintf("t=%s,s=%s", ts, signNMIHeader(secret, ts, missingEventIDBody))
	_, err = PrepareNMI("nmi", missingEventIDBody, secret, missingEventIDHeader)
	require.ErrorIs(t, err, ErrWebhookEventIDMissing)

	invalidNMIBody := []byte(`{"event_id":"evt_123"`)
	invalidNMIHeader := fmt.Sprintf("t=%s,s=%s", ts, signNMIHeader(secret, ts, invalidNMIBody))
	_, err = PrepareNMI("nmi", invalidNMIBody, secret, invalidNMIHeader)
	require.ErrorIs(t, err, ErrWebhookPayloadInvalid)

	_, err = PrepareNMI("nmi", body, secret, "")
	require.True(t, errors.Is(err, ErrNMIWebhookSignatureMissing))
}

func signNMIHeader(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "." + string(body)))
	return hex.EncodeToString(mac.Sum(nil))
}
