package service

import (
	"net/http"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/api"
)

// Catalog refusals (#983). Status and code are the contract; messages are not.
var (
	ErrProductNotFound  = apperr.New(http.StatusNotFound, "product_not_found", "product not found")
	ErrPriceNotFound    = apperr.New(http.StatusNotFound, "price_not_found", "price not found")
	ErrPriceKeyNotFound = apperr.New(http.StatusNotFound, "price_key_not_found", "price key not found")
	// ErrCatalogConflict is a unique-constraint collision on a catalog write.
	ErrCatalogConflict = apperr.New(http.StatusConflict, api.CodeResourceConflict, "a resource with these attributes already exists")
)

// productLookup, priceLookup and catalogWrite translate driver outcomes once,
// at the facade, so handlers classify by type and raw driver text never leaves.
func productLookup(err error) error {
	if db.IsNotFound(err) {
		return ErrProductNotFound
	}
	return catalogWrite(err)
}

func priceLookup(err error) error {
	if db.IsNotFound(err) {
		return ErrPriceNotFound
	}
	return catalogWrite(err)
}

func catalogWrite(err error) error {
	if db.IsUniqueViolation(err) {
		return ErrCatalogConflict
	}
	return err
}
