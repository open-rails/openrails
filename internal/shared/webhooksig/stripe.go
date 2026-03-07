package webhooksig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// VerifyStripeSignature verifies a Stripe webhook signature header.
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

// ParseStripeSignatureHeader parses the Stripe-Signature header.
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
