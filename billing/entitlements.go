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

// EntitlementListParams reads the entitlements customers hold at At (zero:
// now), ordered by customer, then by key in byte order. CustomerIDs names 1
// to MaxBatchItems customers; with none, Entitlements names exactly one key
// and the answer is the customers holding it. Entitlements keeps only those
// keys (at most MaxBatchItems): a key absent from the answer is not held.
// Prefix keeps only keys under a byte prefix, which OpenRails gives no
// meaning; its last byte must be printable ASCII (0x21-0x7E).
type EntitlementListParams struct {
	PageRequest
	CustomerIDs  []CustomerID
	Entitlements []string
	Prefix       string
	At           time.Time
}

// CustomerEntitlement is one key a customer holds: a key of a product they
// hold. Quantity is the most seats a held per-seat product grants it; null
// when none is per seat.
type CustomerEntitlement struct {
	CustomerID  CustomerID `json:"customer_id"`
	Entitlement string     `json:"entitlement"`
	Quantity    *int       `json:"quantity"`
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

// ProductAccessListParams pages product-access windows, newest first, of
// the named customers and products (each 1 to MaxBatchItems; none: any);
// LiveOnly keeps those live now. IDs instead reads 1 to MaxBatchItems named
// windows in one page; unknown ones are absent.
type ProductAccessListParams struct {
	PageRequest
	CustomerIDs []CustomerID
	ProductIDs  []ProductID
	LiveOnly    bool
	IDs         []ProductAccessID
}
