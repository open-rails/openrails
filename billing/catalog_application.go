package billing

// CatalogApplicationReceipt is the result of applying a catalog document:
// the revisions before and after, and how many products and prices changed.
// Replayed is true when the same document was already applied.
type CatalogApplicationReceipt struct {
	ApplicationID   string    `json:"application_id"`
	CatalogID       CatalogID `json:"catalog_id"`
	BaseRevision    int64     `json:"base_revision"`
	AppliedRevision int64     `json:"applied_revision"`
	Replayed        bool      `json:"replayed"`
	ProductsChanged int       `json:"products_changed"`
	PricesChanged   int       `json:"prices_changed"`
}

// CatalogRevision is the merchant's catalog revision, which every catalog
// write advances, and whether this deployment accepts catalog writes.
type CatalogRevision struct {
	Revision      int64 `json:"revision"`
	WritesAllowed bool  `json:"writes_allowed"`
}

// CodeCatalogDeclared refuses a catalog write while Config.Catalog declares
// the catalog (HTTP 405): the next boot would overwrite it. Change the
// declaration and restart instead.
const CodeCatalogDeclared = "catalog_declared"

var ErrCatalogDeclared error = newCodedError(CodeCatalogDeclared, ErrInvalid)
