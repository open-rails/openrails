package billing

import (
	"time"

	"github.com/google/uuid"
)

// ProductAccessID names one product-access window (pa_).
type ProductAccessID uuid.UUID

const ProductAccessIDPrefix = "pa_"

func ParseProductAccessID(s string) (ProductAccessID, error) {
	u, err := parsePrefixedID("product access", ProductAccessIDPrefix, s)
	return ProductAccessID(u), err
}

func (id ProductAccessID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id ProductAccessID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id ProductAccessID) String() string {
	return formatPrefixedID(ProductAccessIDPrefix, uuid.UUID(id))
}
func (id ProductAccessID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *ProductAccessID) UnmarshalText(text []byte) error {
	parsed, err := ParseProductAccessID(string(text))
	*id = parsed
	return err
}

// MaxEntitlementChecks bounds the keys of one entitlement check.
const MaxEntitlementChecks = 100

// Bounds of the prefixes of one entitlement check and of the keys answered
// under each.
const (
	MaxEntitlementPrefixes  = 10
	DefaultHeldEntitlements = 1000
	MaxHeldEntitlements     = 10000
)

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
// Quantities answers every requested key with the most seats a held per-seat
// product grants it: null when it is not held per seat. Prefixes carry none.
type EntitlementCheck struct {
	Entitlements map[string]bool             `json:"entitlements"`
	Quantities   map[string]*int             `json:"quantities"`
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

// CustomerEntitlementListParams pages the keys a customer holds at At (zero:
// now), in byte order, optionally only those under Prefix (bytes, as
// CheckEntitlementsParams.Prefixes).
type CustomerEntitlementListParams struct {
	PageRequest
	Prefix string
	At     time.Time
}

// CustomerEntitlement is one key a customer holds: a key of a product they
// hold.
type CustomerEntitlement struct {
	Entitlement string `json:"entitlement"`
}

// GetEffectiveTiersParams asks the tier each of 1 to MaxBatchItems customers
// holds in Group.
type GetEffectiveTiersParams struct {
	Group       string       `json:"group"`
	CustomerIDs []CustomerID `json:"customer_ids"`
}

// EffectiveTierLookup answers every requested customer with the tier they
// hold in the group: the highest-ranked product whose entitlements they hold.
// A customer holding none, or unknown to the merchant, is null; the host
// applies its default.
type EffectiveTierLookup struct {
	Tiers map[CustomerID]*Tier `json:"tiers"`
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

// ProductAccessSourceType is what a product-access window came from.
type ProductAccessSourceType string

const (
	ProductAccessSourcePurchase     ProductAccessSourceType = "purchase"
	ProductAccessSourceSubscription ProductAccessSourceType = "subscription"
	// ProductAccessSourceGrace is the access a failed renewal keeps while it
	// is retried.
	ProductAccessSourceGrace ProductAccessSourceType = "grace"
	// ProductAccessSourceGrant is a free grant by staff or an import.
	ProductAccessSourceGrant ProductAccessSourceType = "grant"
)

// GrantReason is why a product was granted free.
type GrantReason string

const (
	GrantReasonComp   GrantReason = "comp"
	GrantReasonStaff  GrantReason = "staff"
	GrantReasonImport GrantReason = "import"
	// GrantReasonMigration marks access carried over from per-key
	// entitlement windows.
	GrantReasonMigration GrantReason = "migration"
)

// ProductAccessGrant is one window of a product a customer holds: they hold
// the product's current keys while it is live. SourceID is the source's own
// wire id (pay_ for a purchase, sub_ for a subscription period or grace, the
// idempotency key of a free grant). GrantReason, GrantedBy and Note are set
// for a free grant.
type ProductAccessGrant struct {
	ID          ProductAccessID         `json:"id"`
	CustomerID  CustomerID              `json:"customer_id"`
	ProductID   ProductID               `json:"product_id"`
	ProductKey  string                  `json:"product_key"`
	ProductName string                  `json:"product_name"`
	SourceType  ProductAccessSourceType `json:"source_type"`
	SourceID    string                  `json:"source_id"`
	PaymentID   *PaymentID              `json:"payment_id"`
	// Quantity is the seats the window gives: its per-seat subscription's;
	// null otherwise.
	Quantity     *int         `json:"quantity"`
	GrantReason  *GrantReason `json:"grant_reason"`
	GrantedBy    *string      `json:"granted_by"`
	Note         *string      `json:"note"`
	Status       string       `json:"status"`
	StartsAt     time.Time    `json:"starts_at"`
	EndsAt       *time.Time   `json:"ends_at"`
	RevokedAt    *time.Time   `json:"revoked_at"`
	RevokeReason *string      `json:"revoke_reason"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
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
// the request named: whether the customer holds it, and the most seats a live
// window gives (null when not held per seat).
type ProductAccessCheck struct {
	Access     map[string]bool `json:"access"`
	Quantities map[string]*int `json:"quantities"`
}

// CreateProductAccessParams grants a customer a product free. At most one of
// Hours and EndsAt: Hours extends the customer's access to the product, from
// the end of their latest live window of it (or now); EndsAt ends this grant
// at a fixed instant from now; neither grants it indefinitely (it needs
// merchant:access:grant-permanent). Reason defaults to staff; Note is kept
// with the grant.
type CreateProductAccessParams struct {
	CustomerID CustomerID  `json:"customer_id"`
	ProductID  ProductID   `json:"product_id"`
	Hours      *int        `json:"hours"`
	EndsAt     *time.Time  `json:"ends_at"`
	Reason     GrantReason `json:"reason,omitempty"`
	Note       *string     `json:"note"`
}

// CreateProductAccessBatchParams grants 1 to MaxBatchItems products, across
// any customers, in one transaction. IdempotencyKey (the Idempotency-Key
// header) makes a retry replay the first answer; reusing it for other items
// is ErrIdempotencyKeyReused.
type CreateProductAccessBatchParams struct {
	IdempotencyKey string                      `json:"-"`
	Items          []CreateProductAccessParams `json:"items"`
}

// CreateProductAccessBatchResult is every grant, in request order.
type CreateProductAccessBatchResult struct {
	Items []ProductAccessGrant `json:"items"`
}

// ProductAccessListParams pages a customer's product-access windows, newest
// first; LiveOnly keeps those live now. IDs instead reads 1 to MaxBatchItems
// of the customer's named windows in one page; unknown ones are absent.
type ProductAccessListParams struct {
	PageRequest
	LiveOnly bool
	IDs      []ProductAccessID
}
