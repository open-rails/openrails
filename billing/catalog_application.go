package billing

import (
	"encoding/json"
	"time"
)

// ApplyCatalogParams is how a catalog document applies. Force overwrites the
// fields an edit set and takes them; without it an object with such a field
// is skipped and reported in Conflicts.
type ApplyCatalogParams struct {
	Force bool
}

// CatalogApplicationReceipt is the result of applying a catalog document:
// the revisions before and after, and how many products and prices changed.
// Replayed is true when the same document was already applied.
// EntitlementChanges lists how the edit changed existing products' keys;
// Changes lists every object the document changed and Conflicts every object
// it skipped. They are returned once and are empty on a replay. A document
// with conflicts is not recorded as applied: applying it again retries them.
type CatalogApplicationReceipt struct {
	ApplicationID      string              `json:"application_id"`
	BaseRevision       int64               `json:"base_revision"`
	AppliedRevision    int64               `json:"applied_revision"`
	Replayed           bool                `json:"replayed"`
	ProductsChanged    int                 `json:"products_changed"`
	PricesChanged      int                 `json:"prices_changed"`
	EntitlementChanges []EntitlementChange `json:"entitlement_changes"`
	Changes            []CatalogChange     `json:"changes"`
	Conflicts          []CatalogConflict   `json:"conflicts"`
}

// CatalogObjectKind is a catalog object a document declares: a product, one
// of its price keys or a meter. Each applies whole or not at all.
type CatalogObjectKind string

const (
	CatalogObjectProduct CatalogObjectKind = "product"
	CatalogObjectPrice   CatalogObjectKind = "price"
	CatalogObjectMeter   CatalogObjectKind = "meter"
)

// CatalogChange is one object a document changed: the fields it set and the
// object's revision after. ProductKey names a price's product.
type CatalogChange struct {
	Object     CatalogObjectKind `json:"object"`
	Key        string            `json:"key"`
	ProductKey *string           `json:"product_key"`
	Fields     []string          `json:"fields"`
	Revision   int64             `json:"revision"`
}

// CatalogConflict is one object a document skipped: none of its changes
// applied, because each of Fields names a value an edit set differently.
// Fix the document to agree, drop the fields from it, or apply with force.
type CatalogConflict struct {
	Object     CatalogObjectKind `json:"object"`
	Key        string            `json:"key"`
	ProductKey *string           `json:"product_key"`
	// Currency is a price's live currency, which its money fields are in.
	Currency *string                `json:"currency"`
	Fields   []CatalogFieldConflict `json:"fields"`
}

// CatalogFieldConflict is one field a document names whose live value an
// edit set: the document's value, the live one (each as the document writes
// it), and who set the live value and when.
type CatalogFieldConflict struct {
	Field     string          `json:"field"`
	FileValue json.RawMessage `json:"file_value"`
	LiveValue json.RawMessage `json:"live_value"`
	SetBy     string          `json:"set_by"`
	SetAt     time.Time       `json:"set_at"`
}

// EntitlementChange is how one catalog edit changed an existing product's
// keys. Holders of the product gain Added and lose Removed at once, unless
// another product they hold grants a removed key.
type EntitlementChange struct {
	ProductID  ProductID `json:"product_id"`
	ProductKey string    `json:"product_key"`
	Added      []string  `json:"added"`
	Removed    []string  `json:"removed"`
	// Holders is how many customers held the product when it changed: whose
	// access the edit changed.
	Holders int64 `json:"holders"`
}

// CatalogRevision is the merchant's catalog revision, which every catalog
// write advances.
type CatalogRevision struct {
	Revision int64 `json:"revision"`
}
