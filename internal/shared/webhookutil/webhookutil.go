package webhookutil

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const canonicalMobiusProvider = "mobius"

var (
	ErrNMIWebhookSecretMissing    = errors.New("nmi webhook secret not configured")
	ErrNMIWebhookSignatureMissing = errors.New("missing webhook signature")
	ErrNMIWebhookSignatureInvalid = errors.New("invalid webhook signature")
)

func CanonicalProvider(provider string) string {
	provider = strings.Trim(strings.ToLower(provider), " /")
	if provider == "nmi" {
		return canonicalMobiusProvider
	}
	return provider
}

func ParseStripeEventMeta(body []byte) (string, string, error) {
	var payload struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(payload.ID) == "" || strings.TrimSpace(payload.Type) == "" {
		return "", "", fmt.Errorf("missing event id or type")
	}
	return payload.ID, payload.Type, nil
}

func ComputeUniqueKey(provider, eventID, eventType string, body []byte) string {
	provider = CanonicalProvider(provider)
	eventID = strings.TrimSpace(eventID)
	if eventID != "" {
		return fmt.Sprintf("webhook:%s:%s", provider, eventID)
	}
	hash := sha256.Sum256(append([]byte(provider+"|"+eventType+"|"), body...))
	return fmt.Sprintf("webhook:%s:%s", provider, hex.EncodeToString(hash[:8]))
}

func NormalizeCCBillPayload(body []byte) ([]byte, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return body, nil
	}
	if body[0] == '{' || body[0] == '[' {
		return body, nil
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	payload := make(map[string]string, len(values))
	for key, val := range values {
		if len(val) > 0 {
			payload[key] = val[0]
		}
	}
	return json.Marshal(payload)
}

func VerifyStripeSignature(secret, header string, body []byte, tolerance time.Duration) error {
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
		if now-tsInt > int64(tolerance.Seconds()) || tsInt-now > int64(tolerance.Seconds()) {
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

func VerifyNMISignature(secret, header string, body []byte) error {
	timestamp, signature, err := ParseNMISignatureHeader(header)
	if err != nil {
		return err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "." + string(body)))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		return fmt.Errorf("invalid webhook signature")
	}

	return nil
}

func ParseNMISignatureHeader(header string) (string, string, error) {
	var ts string
	var sig string

	parts := strings.Split(header, ",")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}

		switch strings.TrimSpace(kv[0]) {
		case "t":
			ts = strings.TrimSpace(kv[1])
		case "s":
			sig = strings.TrimSpace(kv[1])
		}
	}

	if ts == "" || sig == "" {
		return "", "", fmt.Errorf("unrecognized webhook signature format")
	}

	return ts, sig, nil
}

func ValidateNMISignature(secret string, body []byte, phpHeader string, legacyHeaders []string, verifyLegacy func(string) error) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", ErrNMIWebhookSecretMissing
	}

	phpHeader = strings.TrimSpace(phpHeader)
	if phpHeader != "" {
		if err := VerifyNMISignature(secret, phpHeader, body); err != nil {
			return "", fmt.Errorf("%w: %v", ErrNMIWebhookSignatureInvalid, err)
		}
		return phpHeader, nil
	}

	for _, header := range legacyHeaders {
		header = strings.TrimSpace(header)
		if header == "" {
			continue
		}
		if err := verifyLegacy(header); err != nil {
			return "", fmt.Errorf("%w: %v", ErrNMIWebhookSignatureInvalid, err)
		}
		return header, nil
	}

	return "", ErrNMIWebhookSignatureMissing
}
