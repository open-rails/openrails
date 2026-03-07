package api

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ID prefixes for different resource types (Stripe-like pattern).
// These prefixes only appear in the API layer - database stores plain UUIDs.
const (
	PrefixProduct         = "prod_"
	PrefixPrice           = "price_"
	PrefixSubscription    = "sub_"
	PrefixPayment         = "pay_"
	PrefixPaymentMethod   = "pm_"
	PrefixInvoice         = "inv_"
	PrefixCheckoutSession = "cs_"
	PrefixUser            = "usr_"
	PrefixEvent           = "evt_"
	PrefixAdminGrant      = "ag_"
)

// FormatProductID formats a UUID as a product ID (prod_xxx)
func FormatProductID(id uuid.UUID) string {
	return PrefixProduct + id.String()
}

// FormatPriceID formats a UUID as a price ID (price_xxx)
func FormatPriceID(id uuid.UUID) string {
	return PrefixPrice + id.String()
}

// FormatSubscriptionID formats a UUID as a subscription ID (sub_xxx)
func FormatSubscriptionID(id uuid.UUID) string {
	return PrefixSubscription + id.String()
}

// FormatPaymentID formats a UUID as a payment ID (pay_xxx)
func FormatPaymentID(id uuid.UUID) string {
	return PrefixPayment + id.String()
}

// FormatPaymentMethodID formats a UUID as a payment method ID (pm_xxx)
func FormatPaymentMethodID(id uuid.UUID) string {
	return PrefixPaymentMethod + id.String()
}

// FormatInvoiceID formats a UUID as an invoice ID (inv_xxx)
func FormatInvoiceID(id uuid.UUID) string {
	return PrefixInvoice + id.String()
}

// FormatCheckoutSessionID formats a UUID as a checkout session ID (cs_xxx)
func FormatCheckoutSessionID(id uuid.UUID) string {
	return PrefixCheckoutSession + id.String()
}

// FormatUserID formats a user ID with the usr_ prefix
// Note: User IDs may not be UUIDs, so this accepts a string
func FormatUserID(id string) string {
	return PrefixUser + id
}

// FormatEventID formats a UUID as an event ID (evt_xxx)
func FormatEventID(id uuid.UUID) string {
	return PrefixEvent + id.String()
}

// FormatAdminGrantID formats a UUID as an admin grant ID (ag_xxx)
func FormatAdminGrantID(id uuid.UUID) string {
	return PrefixAdminGrant + id.String()
}

// ParseProductID parses a prefixed product ID and returns the UUID
func ParseProductID(id string) (uuid.UUID, error) {
	return parseID(id, PrefixProduct, "product")
}

// ParsePriceID parses a prefixed price ID and returns the UUID
func ParsePriceID(id string) (uuid.UUID, error) {
	return parseID(id, PrefixPrice, "price")
}

// ParseSubscriptionID parses a prefixed subscription ID and returns the UUID
func ParseSubscriptionID(id string) (uuid.UUID, error) {
	return parseID(id, PrefixSubscription, "subscription")
}

// ParsePaymentID parses a prefixed payment ID and returns the UUID
func ParsePaymentID(id string) (uuid.UUID, error) {
	return parseID(id, PrefixPayment, "payment")
}

// ParsePaymentMethodID parses a prefixed payment method ID and returns the UUID
func ParsePaymentMethodID(id string) (uuid.UUID, error) {
	return parseID(id, PrefixPaymentMethod, "payment_method")
}

// ParseInvoiceID parses a prefixed invoice ID and returns the UUID
func ParseInvoiceID(id string) (uuid.UUID, error) {
	return parseID(id, PrefixInvoice, "invoice")
}

// ParseCheckoutSessionID parses a prefixed checkout session ID and returns the UUID
func ParseCheckoutSessionID(id string) (uuid.UUID, error) {
	return parseID(id, PrefixCheckoutSession, "checkout_session")
}

// ParseUserID parses a prefixed user ID and returns the raw ID string
func ParseUserID(id string) (string, error) {
	if !strings.HasPrefix(id, PrefixUser) {
		return "", fmt.Errorf("invalid user ID: expected prefix '%s'", PrefixUser)
	}
	return strings.TrimPrefix(id, PrefixUser), nil
}

// ParseEventID parses a prefixed event ID and returns the UUID
func ParseEventID(id string) (uuid.UUID, error) {
	return parseID(id, PrefixEvent, "event")
}

// parseID is a helper that parses a prefixed ID string into a UUID
func parseID(id, prefix, resourceType string) (uuid.UUID, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return uuid.Nil, fmt.Errorf("invalid %s ID: value is empty", resourceType)
	}

	if parsed, err := uuid.Parse(trimmed); err == nil {
		return parsed, nil
	}

	if after, ok := strings.CutPrefix(trimmed, prefix); ok {
		rawID := after
		if parsed, err := uuid.Parse(rawID); err == nil {
			return parsed, nil
		}
	}

	return uuid.Nil, fmt.Errorf("invalid %s ID: expected prefix '%s' or valid UUID", resourceType, prefix)
}
