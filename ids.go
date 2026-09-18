package openrails

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Typed resource identifiers. Each kind has exactly one wire spelling:
// OpenRails-minted resources travel as prefixed text (prod_, price_, sub_,
// pay_, pm_, cs_) and host-owned identities (CustomerID, MerchantID) as the
// plain UUID. A typed id marshals to that spelling and refuses any other — a
// missing or wrong prefix, or a non-UUID body, is a decoding error. The zero
// id marshals to "" and "" decodes to the zero id; use IsZero (and the json
// omitzero option) rather than pointers for optional request fields.
//
// Database rows store the bare UUID; UUID() and the conversion ProductID(u)
// are the only boundary between the two.
type (
	// CustomerID is the host's stable subject UUID under the bound merchant.
	CustomerID        uuid.UUID
	ProductID         uuid.UUID
	PriceID           uuid.UUID
	SubscriptionID    uuid.UUID
	PaymentID         uuid.UUID
	PaymentMethodID   uuid.UUID
	CheckoutSessionID uuid.UUID
)

const (
	ProductIDPrefix         = "prod_"
	PriceIDPrefix           = "price_"
	SubscriptionIDPrefix    = "sub_"
	PaymentIDPrefix         = "pay_"
	PaymentMethodIDPrefix   = "pm_"
	CheckoutSessionIDPrefix = "cs_"
)

func ParseProductID(s string) (ProductID, error) {
	u, err := parsePrefixedID("product", ProductIDPrefix, s)
	return ProductID(u), err
}
func ParsePriceID(s string) (PriceID, error) {
	u, err := parsePrefixedID("price", PriceIDPrefix, s)
	return PriceID(u), err
}
func ParseSubscriptionID(s string) (SubscriptionID, error) {
	u, err := parsePrefixedID("subscription", SubscriptionIDPrefix, s)
	return SubscriptionID(u), err
}
func ParsePaymentID(s string) (PaymentID, error) {
	u, err := parsePrefixedID("payment", PaymentIDPrefix, s)
	return PaymentID(u), err
}
func ParsePaymentMethodID(s string) (PaymentMethodID, error) {
	u, err := parsePrefixedID("payment method", PaymentMethodIDPrefix, s)
	return PaymentMethodID(u), err
}
func ParseCheckoutSessionID(s string) (CheckoutSessionID, error) {
	u, err := parsePrefixedID("checkout session", CheckoutSessionIDPrefix, s)
	return CheckoutSessionID(u), err
}

// ParseCustomerID reads the plain UUID spelling of a customer id.
func ParseCustomerID(s string) (CustomerID, error) {
	u, err := parsePrefixedID("customer", "", s)
	return CustomerID(u), err
}

func (id CustomerID) UUID() uuid.UUID        { return uuid.UUID(id) }
func (id ProductID) UUID() uuid.UUID         { return uuid.UUID(id) }
func (id PriceID) UUID() uuid.UUID           { return uuid.UUID(id) }
func (id SubscriptionID) UUID() uuid.UUID    { return uuid.UUID(id) }
func (id PaymentID) UUID() uuid.UUID         { return uuid.UUID(id) }
func (id PaymentMethodID) UUID() uuid.UUID   { return uuid.UUID(id) }
func (id CheckoutSessionID) UUID() uuid.UUID { return uuid.UUID(id) }

func (id CustomerID) IsZero() bool        { return uuid.UUID(id) == uuid.Nil }
func (id ProductID) IsZero() bool         { return uuid.UUID(id) == uuid.Nil }
func (id PriceID) IsZero() bool           { return uuid.UUID(id) == uuid.Nil }
func (id SubscriptionID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id PaymentID) IsZero() bool         { return uuid.UUID(id) == uuid.Nil }
func (id PaymentMethodID) IsZero() bool   { return uuid.UUID(id) == uuid.Nil }
func (id CheckoutSessionID) IsZero() bool { return uuid.UUID(id) == uuid.Nil }

// String is the wire spelling; the zero id is "".
func (id CustomerID) String() string { return formatPrefixedID("", uuid.UUID(id)) }
func (id ProductID) String() string  { return formatPrefixedID(ProductIDPrefix, uuid.UUID(id)) }
func (id PriceID) String() string    { return formatPrefixedID(PriceIDPrefix, uuid.UUID(id)) }
func (id SubscriptionID) String() string {
	return formatPrefixedID(SubscriptionIDPrefix, uuid.UUID(id))
}
func (id PaymentID) String() string { return formatPrefixedID(PaymentIDPrefix, uuid.UUID(id)) }
func (id PaymentMethodID) String() string {
	return formatPrefixedID(PaymentMethodIDPrefix, uuid.UUID(id))
}
func (id CheckoutSessionID) String() string {
	return formatPrefixedID(CheckoutSessionIDPrefix, uuid.UUID(id))
}

func (id CustomerID) MarshalText() ([]byte, error)        { return []byte(id.String()), nil }
func (id ProductID) MarshalText() ([]byte, error)         { return []byte(id.String()), nil }
func (id PriceID) MarshalText() ([]byte, error)           { return []byte(id.String()), nil }
func (id SubscriptionID) MarshalText() ([]byte, error)    { return []byte(id.String()), nil }
func (id PaymentID) MarshalText() ([]byte, error)         { return []byte(id.String()), nil }
func (id PaymentMethodID) MarshalText() ([]byte, error)   { return []byte(id.String()), nil }
func (id CheckoutSessionID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

func (id *CustomerID) UnmarshalText(text []byte) error {
	parsed, err := ParseCustomerID(string(text))
	*id = parsed
	return err
}
func (id *ProductID) UnmarshalText(text []byte) error {
	parsed, err := ParseProductID(string(text))
	*id = parsed
	return err
}
func (id *PriceID) UnmarshalText(text []byte) error {
	parsed, err := ParsePriceID(string(text))
	*id = parsed
	return err
}
func (id *SubscriptionID) UnmarshalText(text []byte) error {
	parsed, err := ParseSubscriptionID(string(text))
	*id = parsed
	return err
}
func (id *PaymentID) UnmarshalText(text []byte) error {
	parsed, err := ParsePaymentID(string(text))
	*id = parsed
	return err
}
func (id *PaymentMethodID) UnmarshalText(text []byte) error {
	parsed, err := ParsePaymentMethodID(string(text))
	*id = parsed
	return err
}
func (id *CheckoutSessionID) UnmarshalText(text []byte) error {
	parsed, err := ParseCheckoutSessionID(string(text))
	*id = parsed
	return err
}

// SourceRef spells a polymorphic entitlement or grant source in its kind's
// wire form, so source_id beside source_type is the id the source resource
// itself carries: subscription and grace sources are SubscriptionIDs, one_off
// and purchase sources are PaymentIDs. Any other source (admin) is the host's
// own declared id, verbatim.
func SourceRef(sourceType, id string) string {
	u, err := uuid.Parse(id)
	if err != nil {
		return id
	}
	switch sourceType {
	case "subscription", "grace":
		return SubscriptionID(u).String()
	case "one_off", "purchase":
		return PaymentID(u).String()
	}
	return id
}

func formatPrefixedID(prefix string, u uuid.UUID) string {
	if u == uuid.Nil {
		return ""
	}
	return prefix + u.String()
}

// parsePrefixedID accepts "" (the zero id) or prefix followed by a canonical
// UUID. It never accepts a bare UUID for a prefixed kind, nor a prefixed
// value for a plain one, so a price id can never be read where a product id
// is expected.
func parsePrefixedID(kind, prefix, s string) (uuid.UUID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return uuid.Nil, nil
	}
	body, ok := strings.CutPrefix(s, prefix)
	if !ok || len(body) != 36 {
		return uuid.Nil, fmt.Errorf("invalid %s id %q: expected %s<uuid>", kind, s, prefix)
	}
	u, err := uuid.Parse(body)
	if err != nil || u == uuid.Nil {
		return uuid.Nil, fmt.Errorf("invalid %s id %q: expected %s<uuid>", kind, s, prefix)
	}
	return u, nil
}
