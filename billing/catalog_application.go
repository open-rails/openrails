package billing

// CatalogApplicationReceipt is the result of applying a catalog document:
// the revisions before and after, and how many products and prices changed.
// Replayed is true when the same document was already applied.
// EntitlementChanges lists how the edit changed existing products' keys; it
// is returned once and is empty on a replay.
type CatalogApplicationReceipt struct {
	ApplicationID      string              `json:"application_id"`
	BaseRevision       int64               `json:"base_revision"`
	AppliedRevision    int64               `json:"applied_revision"`
	Replayed           bool                `json:"replayed"`
	ProductsChanged    int                 `json:"products_changed"`
	PricesChanged      int                 `json:"prices_changed"`
	EntitlementChanges []EntitlementChange `json:"entitlement_changes"`
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

// MaxEntitlementReplacements bounds the pairs of one ReplaceEntitlements call.
const MaxEntitlementReplacements = 100

// ReplaceEntitlementsParams moves every product granting From to grant To
// instead, in one catalog edit. An empty To removes From. A key may not be
// both replaced and a replacement in one call.
type ReplaceEntitlementsParams struct {
	Pairs []EntitlementReplacement `json:"pairs"`
}

// EntitlementReplacement is one key moved across every product granting it.
type EntitlementReplacement struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// CatalogRevision is the merchant's catalog revision, which every catalog
// write advances, and whether this deployment accepts catalog writes.
type CatalogRevision struct {
	Revision      int64 `json:"revision"`
	WritesAllowed bool  `json:"writes_allowed"`
}
