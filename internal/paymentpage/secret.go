// Package paymentpage is the payment page: an order's pay URL on
// the checkout origin (a merchant order's, or a customer's own order's), the
// secret it carries, the page served there and what the secret opens. The secret
// acts as the order's customer, for that order alone, and only on the
// checkout origin (Stripe's client secret).
package paymentpage

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// SecretPrefix marks a pay URL's secret, for secret scanners.
const SecretPrefix = "pus_"

const (
	secretBytes = 32
	secretLen   = len(SecretPrefix) + 43 // base64url of 32 bytes, unpadded
)

// NewSecret mints a pay URL secret and its authentication hash. Attach stores
// the hash on the order and retains the secret in its bounded creation receipt.
func NewSecret() (secret string, hash []byte, err error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	secret = SecretPrefix + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	return secret, sum[:], nil
}

// Hash is the stored form of a presented secret; false when it is not shaped
// like one, so a malformed bearer never reaches the database.
func Hash(secret string) ([]byte, bool) {
	if len(secret) != secretLen || !strings.HasPrefix(secret, SecretPrefix) {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(secret[len(SecretPrefix):])
	if err != nil || len(raw) != secretBytes {
		return nil, false
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:], true
}

// PagePrefix starts every order page's path on the checkout origin.
const PagePrefix = "/pay/"

// PagePath is the page's path for an order on the checkout origin.
func PagePath(order billing.OrderID) string { return PagePrefix + order.String() }

// URL is the pay URL: the page for the order on the checkout origin,
// with the secret in the fragment, which browsers never send to a server.
func URL(origin string, order billing.OrderID, secret string) string {
	return strings.TrimRight(origin, "/") + PagePath(order) + "#" + secret
}
