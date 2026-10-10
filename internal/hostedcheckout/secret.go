// Package hostedcheckout is the hosted checkout: a merchant order's checkout
// URL on the checkout host, the secret it carries, the page served there and
// what the secret opens. The secret acts as the order's customer, for that
// order alone, and only on the checkout host (Stripe's client secret).
package hostedcheckout

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// SecretPrefix marks a checkout URL's secret, for secret scanners.
const SecretPrefix = "cks_"

const (
	secretBytes = 32
	secretLen   = len(SecretPrefix) + 43 // base64url of 32 bytes, unpadded
)

// NewSecret mints a checkout secret and the hash stored for it. The secret
// itself is never stored.
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

// PagePath is the page's path for an order on the checkout host.
func PagePath(order billing.OrderID) string { return "/c/" + order.String() }

// URL is the checkout URL: the page for the order on the checkout origin,
// with the secret in the fragment, which browsers never send to a server.
func URL(origin string, order billing.OrderID, secret string) string {
	return strings.TrimRight(origin, "/") + PagePath(order) + "#" + secret
}
