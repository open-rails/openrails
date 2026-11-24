// Package sigverify holds the HMAC webhook-signature primitives (Stripe, NMI).
// It is a stdlib-only leaf so the public ingestion path (webhookutil) and the
// queued re-verification path (modules/webhooks) share one implementation.
package sigverify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseStripeSignatureHeader extracts the timestamp and the list of v1
// signatures from a Stripe-Signature header ("t=...,v1=...,v1=...").
func ParseStripeSignatureHeader(header string) (string, []string) {
	parts := strings.Split(header, ",")
	var ts string
	sigs := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "t=") {
			ts = strings.TrimPrefix(part, "t=")
			continue
		}
		if strings.HasPrefix(part, "v1=") {
			sigs = append(sigs, strings.TrimPrefix(part, "v1="))
		}
	}
	return ts, sigs
}

// VerifyStripe authenticates a Stripe webhook's HMAC and, when tolerance > 0,
// rejects timestamps outside that window. A non-positive tolerance verifies the
// HMAC only and SKIPS the replay-window check (used by the queued re-verify
// path, where a job may be processed long after delivery).
func VerifyStripe(secret, header string, body []byte, tolerance time.Duration) error {
	timestamp, signatures := ParseStripeSignatureHeader(header)
	if timestamp == "" || len(signatures) == 0 {
		return fmt.Errorf("invalid stripe signature header")
	}
	tsInt, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid stripe signature timestamp")
	}
	if tolerance > 0 {
		now := time.Now().Unix()
		tol := int64(tolerance.Seconds())
		if now-tsInt > tol || tsInt-now > tol {
			return fmt.Errorf("stripe signature timestamp outside tolerance")
		}
	}

	signedPayload := fmt.Sprintf("%s.%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signedPayload))
	expected := hex.EncodeToString(mac.Sum(nil))
	for _, sig := range signatures {
		if hmac.Equal([]byte(expected), []byte(sig)) {
			return nil
		}
	}
	return fmt.Errorf("stripe signature mismatch")
}

// ParseNMISignatureHeader splits an NMI Webhook-Signature header,
// "t=<nonce>,s=<hex>", as NMI's sample reads it: the nonce runs to the last
// ",s=". The nonce is opaque; it only enters the HMAC.
func ParseNMISignatureHeader(header string) (string, string, error) {
	header = strings.TrimSpace(header)
	cut := strings.LastIndex(header, ",s=")
	if !strings.HasPrefix(header, "t=") || cut < len("t=") {
		return "", "", fmt.Errorf("unrecognized webhook signature format")
	}
	nonce, sig := header[len("t="):cut], header[cut+len(",s="):]
	if nonce == "" || sig == "" {
		return "", "", fmt.Errorf("unrecognized webhook signature format")
	}
	return nonce, sig, nil
}

// VerifyNMI authenticates an NMI webhook: HMAC-SHA256 over "<nonce>.<body>".
// NMI's t is a nonce, not a time, so there is no replay window; a replay is
// the same event_id, which webhook deduplication answers once.
func VerifyNMI(secret, header string, body []byte) error {
	nonce, signature, err := ParseNMISignatureHeader(header)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(nonce + "."))
	_, _ = mac.Write(body)
	providedSig, err := hex.DecodeString(strings.ToLower(signature))
	if err != nil {
		return fmt.Errorf("invalid webhook signature")
	}
	if !hmac.Equal(providedSig, mac.Sum(nil)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}
