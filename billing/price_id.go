package billing

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// PriceID identifies immutable financial terms within a merchant's catalog.
// Its spelling is product-key:price-key:v1, with versions starting at one.
// Keys use canonical percent encoding so colons, slashes and Unicode remain
// unambiguous. Archival and reactivation never change an ID.
type PriceID string

// NewPriceID constructs an immutable price identifier from its stable keys.
func NewPriceID(productKey, priceKey string, version int64) (PriceID, error) {
	if strings.TrimSpace(productKey) == "" || strings.TrimSpace(priceKey) == "" || len(productKey) > 255 || len(priceKey) > 255 || version < 1 {
		return "", fmt.Errorf("price ID requires product and price keys of 1-255 bytes and a positive version")
	}
	return PriceID(escapePriceKey(productKey) + ":" + escapePriceKey(priceKey) + ":v" + strconv.FormatInt(version, 10)), nil
}

// ParsePriceID accepts only canonical, versioned identifiers; UUIDs and
// unversioned lookup keys are not price IDs. Empty denotes the optional zero ID.
func ParsePriceID(s string) (PriceID, error) {
	if s == "" {
		return "", nil
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "v") {
		return "", fmt.Errorf("invalid price ID %q: expected product:price:v1", s)
	}
	product, e1 := url.PathUnescape(parts[0])
	price, e2 := url.PathUnescape(parts[1])
	version, e3 := strconv.ParseInt(strings.TrimPrefix(parts[2], "v"), 10, 64)
	id, err := NewPriceID(product, price, version)
	if e1 != nil || e2 != nil || e3 != nil || err != nil || string(id) != s {
		return "", fmt.Errorf("invalid or noncanonical price ID %q", s)
	}
	return id, nil
}

func (id PriceID) IsZero() bool   { return id == "" }
func (id PriceID) String() string { return string(id) }
func (id PriceID) MarshalText() ([]byte, error) {
	if _, err := ParsePriceID(string(id)); err != nil {
		return nil, err
	}
	return []byte(id), nil
}
func (id *PriceID) UnmarshalText(data []byte) error {
	parsed, err := ParsePriceID(string(data))
	if err == nil {
		*id = parsed
	}
	return err
}

func escapePriceKey(key string) string {
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", rune(c)) {
			out.WriteByte(c)
		} else {
			out.WriteByte('%')
			out.WriteByte(hex[c>>4])
			out.WriteByte(hex[c&15])
		}
	}
	return out.String()
}
