package billing

import (
	"time"

	"github.com/google/uuid"
)

// EntitlementID names one entitlement window (ent_).
type EntitlementID uuid.UUID

// ProductAccessID names one product-access grant (pa_).
type ProductAccessID uuid.UUID

const (
	EntitlementIDPrefix   = "ent_"
	ProductAccessIDPrefix = "pa_"
)

func ParseEntitlementID(s string) (EntitlementID, error) {
	u, err := parsePrefixedID("entitlement", EntitlementIDPrefix, s)
	return EntitlementID(u), err
}

func ParseProductAccessID(s string) (ProductAccessID, error) {
	u, err := parsePrefixedID("product access", ProductAccessIDPrefix, s)
	return ProductAccessID(u), err
}

func (id EntitlementID) UUID() uuid.UUID   { return uuid.UUID(id) }
func (id EntitlementID) IsZero() bool      { return uuid.UUID(id) == uuid.Nil }
func (id EntitlementID) String() string    { return formatPrefixedID(EntitlementIDPrefix, uuid.UUID(id)) }
func (id ProductAccessID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id ProductAccessID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id ProductAccessID) String() string {
	return formatPrefixedID(ProductAccessIDPrefix, uuid.UUID(id))
}
func (id EntitlementID) MarshalText() ([]byte, error)   { return []byte(id.String()), nil }
func (id ProductAccessID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *EntitlementID) UnmarshalText(text []byte) error {
	parsed, err := ParseEntitlementID(string(text))
	*id = parsed
	return err
}
func (id *ProductAccessID) UnmarshalText(text []byte) error {
	parsed, err := ParseProductAccessID(string(text))
	*id = parsed
	return err
}

// EntitlementSourceType is what a grant, and the entitlement windows it
// projects, came from.
type EntitlementSourceType string

const (
	EntitlementSourcePurchase     EntitlementSourceType = "purchase"
	EntitlementSourceSubscription EntitlementSourceType = "subscription"
	EntitlementSourceAdmin        EntitlementSourceType = "admin"
	// EntitlementSourceGrace is the access a failed renewal keeps while it
	// is retried.
	EntitlementSourceGrace EntitlementSourceType = "grace"
)

// EntitlementRecord is one entitlement window: the projection of one grant
// for one entitlement key. SourceID is the source's own wire id (sub_ for a
// subscription or grace source, pay_ for a purchase, the grant's own id for an
// admin grant).
type EntitlementRecord struct {
	ID           EntitlementID         `json:"id"`
	CustomerID   CustomerID            `json:"customer_id"`
	Entitlement  string                `json:"entitlement"`
	StartsAt     time.Time             `json:"starts_at"`
	EndsAt       *time.Time            `json:"ends_at"`
	SourceType   EntitlementSourceType `json:"source_type"`
	SourceID     string                `json:"source_id"`
	RevokedAt    *time.Time            `json:"revoked_at"`
	RevokeReason *string               `json:"revoke_reason"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
}

// MaxEntitlementLookupCustomers bounds one ListEntitlements call.
const MaxEntitlementLookupCustomers = 500

// MaxEntitlementChecks bounds the keys of one entitlement check.
const MaxEntitlementChecks = 100

// Bounds of the prefixes of one entitlement check and of the keys answered
// under each.
const (
	MaxEntitlementPrefixes  = 10
	DefaultHeldEntitlements = 1000
	MaxHeldEntitlements     = 10000
)

// EntitlementListParams reads the active entitlements of up to
// MaxEntitlementLookupCustomers customers at At (zero: now).
type EntitlementListParams struct {
	CustomerIDs []CustomerID `json:"customer_ids"`
	At          time.Time    `json:"at,omitzero"`
}

// EntitlementLookup is the active entitlements of each requested customer;
// a customer with none maps to an empty list.
type EntitlementLookup struct {
	Customers map[CustomerID][]EntitlementRecord `json:"customers"`
}

// CheckEntitlementsParams asks which of up to MaxEntitlementChecks keys a
// customer holds at At (zero: now), and which keys they hold under each of up
// to MaxEntitlementPrefixes byte prefixes: at most PrefixLimit per prefix
// (zero: DefaultHeldEntitlements, at most MaxHeldEntitlements). A prefix is
// bytes, not grammar: OpenRails gives it no meaning. Its last byte must be
// printable ASCII (0x21-0x7E). At least one key or prefix is required.
type CheckEntitlementsParams struct {
	Entitlements []string  `json:"entitlements"`
	Prefixes     []string  `json:"prefixes,omitempty"`
	PrefixLimit  int       `json:"prefix_limit,omitempty"`
	At           time.Time `json:"at,omitzero"`
}

// EntitlementCheck answers every requested key, and every requested prefix in
// Held ({} when none was asked).
type EntitlementCheck struct {
	Entitlements map[string]bool             `json:"entitlements"`
	Held         map[string]HeldEntitlements `json:"held"`
}

// HeldEntitlements is the keys a customer holds under one prefix, in byte
// order. Truncated: more were held than the limit returned.
type HeldEntitlements struct {
	Keys      []string `json:"keys"`
	Truncated bool     `json:"truncated"`
}

// EntitlementCustomerListParams pages the customers holding one entitlement
// at At (zero: now).
type EntitlementCustomerListParams struct {
	PageRequest
	At time.Time
}

// CreateEntitlementParams grants an admin-sourced entitlement window. Omit
// both Hours and EndsAt for an indefinite grant (it needs
// merchant:access:grant-permanent); Hours extends the customer's finite
// timeline, EndsAt fixes this grant's own end.
type CreateEntitlementParams struct {
	Entitlement string     `json:"entitlement"`
	Hours       *int       `json:"hours"`
	EndsAt      *time.Time `json:"ends_at"`
}

// EffectiveTier is the tier a customer holds in a tier group: the
// highest-ranked product whose entitlements the customer holds. Tier is nil
// when they hold none; the host applies its default.
type EffectiveTier struct {
	Group string `json:"group"`
	Tier  *Tier  `json:"tier"`
}

// Tier is one product of a tier group. Entitlement is its immutable key;
// DisplayName is for display only.
type Tier struct {
	Entitlement string    `json:"entitlement"`
	DisplayName string    `json:"display_name"`
	TierRank    int       `json:"tier_rank"`
	ProductID   ProductID `json:"product_id"`
	ProductKey  string    `json:"product_key"`
}

// ProductAccessGrant is one product a customer has access to. SourceID
// follows the EntitlementRecord rule.
type ProductAccessGrant struct {
	ID           ProductAccessID       `json:"id"`
	CustomerID   CustomerID            `json:"customer_id"`
	ProductID    ProductID             `json:"product_id"`
	ProductKey   string                `json:"product_key"`
	ProductName  string                `json:"product_name"`
	SourceType   EntitlementSourceType `json:"source_type"`
	SourceID     string                `json:"source_id"`
	PaymentID    *PaymentID            `json:"payment_id"`
	Status       string                `json:"status"`
	StartsAt     time.Time             `json:"starts_at"`
	EndsAt       *time.Time            `json:"ends_at"`
	RevokedAt    *time.Time            `json:"revoked_at"`
	RevokeReason *string               `json:"revoke_reason"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
}

// MaxProductAccessChecks bounds one product-access check.
const MaxProductAccessChecks = 100

// CheckProductAccessParams asks about 1 to MaxProductAccessChecks products,
// named by exactly one of ProductIDs and ProductKeys.
type CheckProductAccessParams struct {
	ProductIDs  []ProductID `json:"product_ids"`
	ProductKeys []string    `json:"product_keys"`
}

// ProductAccessCheck answers every requested product, keyed by the id or key
// the request named.
type ProductAccessCheck struct {
	Access map[string]bool `json:"access"`
}

// CreateProductAccessParams grants a customer access to a product until
// EndsAt (nil: indefinitely; it needs merchant:access:grant-permanent). One
// admin's grant of a product to a customer is made once: a repeat answers the
// existing grant.
type CreateProductAccessParams struct {
	CustomerID CustomerID `json:"customer_id"`
	ProductID  ProductID  `json:"product_id"`
	EndsAt     *time.Time `json:"ends_at"`
}

// CreateProductAccessBatchParams grants 1 to MaxBatchItems product accesses,
// across any customers, in one transaction.
type CreateProductAccessBatchParams struct {
	Items []CreateProductAccessParams `json:"items"`
}

// CreateProductAccessBatchResult is every grant, in request order.
type CreateProductAccessBatchResult struct {
	Items []ProductAccessGrant `json:"items"`
}
