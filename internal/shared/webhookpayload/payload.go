package webhookpayload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const canonicalMobiusProvider = "mobius"

// CanonicalProvider normalizes webhook provider names to the forward-only runtime form.
func CanonicalProvider(provider string) string {
	provider = strings.Trim(strings.ToLower(provider), " /")
	if provider == "nmi" {
		return canonicalMobiusProvider
	}
	return provider
}

// ParseStripeEventMeta extracts the event id and type from a Stripe webhook payload.
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

// ComputeUniqueKey returns the canonical dedupe key for a webhook payload.
func ComputeUniqueKey(provider, eventID, eventType string, body []byte) string {
	provider = CanonicalProvider(provider)
	eventID = strings.TrimSpace(eventID)
	if eventID != "" {
		return fmt.Sprintf("webhook:%s:%s", provider, eventID)
	}
	hash := sha256.Sum256(append([]byte(provider+"|"+eventType+"|"), body...))
	return fmt.Sprintf("webhook:%s:%s", provider, hex.EncodeToString(hash[:8]))
}

// NormalizeCCBillPayload canonicalizes CCBill form-encoded webhooks into JSON bytes.
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
