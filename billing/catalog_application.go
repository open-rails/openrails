package billing

import (
	"github.com/open-rails/openrails/catalog"
)

type CatalogApplyParams = catalog.Application
type CatalogApplyProduct = catalog.ApplyProduct
type CatalogApplyPrice = catalog.ApplyPrice
type CatalogApplyMeter = catalog.ApplyMeter
type CatalogField[T any] = catalog.Field[T]

func CatalogValue[T any](v T) CatalogField[T] { return catalog.Value(v) }
func CatalogNull[T any]() CatalogField[T]     { return catalog.Null[T]() }

type CatalogApplicationReceipt struct {
	ApplicationID   string `json:"application_id"`
	CatalogID       string `json:"catalog_id"`
	BaseRevision    int64  `json:"base_revision"`
	AppliedRevision int64  `json:"applied_revision"`
	Replayed        bool   `json:"replayed"`
	ProductsChanged int    `json:"products_changed"`
	PricesChanged   int    `json:"prices_changed"`
}

type CatalogRevision struct {
	Revision      int64 `json:"revision"`
	WritesAllowed bool  `json:"writes_allowed"`
}

// CodeCatalogDeclared refuses a catalog write while Config.Catalog declares
// the catalog (HTTP 405): the next boot would overwrite it. Change the
// declaration and restart instead.
const CodeCatalogDeclared = "catalog_declared"

var ErrCatalogDeclared error = newCodedError(CodeCatalogDeclared, ErrInvalid)

func ParseCatalogApplicationYAML(raw []byte) (*CatalogApplyParams, error) {
	return catalog.ParseApplicationYAML(raw)
}
