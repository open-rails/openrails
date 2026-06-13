// Package sigverify holds the HMAC webhook-signature primitives for the payment
// processors OpenRails ingests (Stripe, NMI). It is a dependency-free leaf
// package (stdlib only) so it can be the SINGLE source of truth shared by both
// the public ingestion path (internal/shared/webhookutil) and the queued
// re-verification path (internal/modules/webhooks) without an import cycle —
// previously each side carried its own copy of this security-critical code,
// which had already drifted (the queued NMI copy silently dropped the
// replay-window check the ingestion copy enforced).
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

// ParseNMISignatureHeader extracts the timestamp and signature from an NMI
// webhook signature header ("t=...,s=...").
func ParseNMISignatureHeader(header string) (string, string, error) {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.TrimSpace(kv[0]) {
		case "t":
			ts = strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		case "s":
			sig = strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		}
	}
	if ts == "" || sig == "" {
		return "", "", fmt.Errorf("unrecognized webhook signature format")
	}
	return ts, sig, nil
}

// VerifyNMI authenticates an NMI webhook's HMAC and, when tolerance > 0, rejects
// timestamps outside that window. A non-positive tolerance verifies the HMAC
// only and SKIPS the replay-window check.
func VerifyNMI(secret, header string, body []byte, tolerance time.Duration) error {
	timestamp, signature, err := ParseNMISignatureHeader(header)
	if err != nil {
		return err
	}
	tsInt, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid webhook signature timestamp")
	}
	if tolerance > 0 {
		now := time.Now().Unix()
		tol := int64(tolerance.Seconds())
		if now-tsInt > tol || tsInt-now > tol {
			return fmt.Errorf("webhook signature timestamp outside tolerance")
		}
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "." + string(body)))
	expectedSig := mac.Sum(nil)
	providedSig, err := hex.DecodeString(strings.ToLower(signature))
	if err != nil {
		return fmt.Errorf("invalid webhook signature")
	}
	if !hmac.Equal(providedSig, expectedSig) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}
