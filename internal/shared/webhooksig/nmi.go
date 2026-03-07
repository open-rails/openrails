package webhooksig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNMIWebhookSecretMissing indicates that NMI webhook verification cannot run without a secret.
	ErrNMIWebhookSecretMissing = errors.New("nmi webhook secret not configured")
	// ErrNMIWebhookSignatureMissing indicates that no supported signature header was provided.
	ErrNMIWebhookSignatureMissing = errors.New("missing webhook signature")
	// ErrNMIWebhookSignatureInvalid indicates that the provided signature failed verification.
	ErrNMIWebhookSignatureInvalid = errors.New("invalid webhook signature")
)

// VerifyNMISignature verifies a PHP-style NMI webhook signature header.
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

// ParseNMISignatureHeader parses the PHP-style Webhook-Signature header.
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

// ValidateNMISignature validates NMI signatures with PHP-style precedence over legacy headers.
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
