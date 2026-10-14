package service

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Catalog refusals. Status and code are the contract; messages are not.
var (
	ErrProductNotFound  = apperr.New(http.StatusNotFound, "product_not_found", "product not found")
	ErrPriceNotFound    = apperr.New(http.StatusNotFound, "price_not_found", "price not found")
	ErrPriceKeyNotFound = apperr.New(http.StatusNotFound, "price_key_not_found", "price key not found")
	// ErrPriceKeyCadenceConflict: a defaulted price key is held by a price on
	// another cadence (billing.ErrPriceKeyCadenceConflict).
	ErrPriceKeyCadenceConflict = apperr.New(http.StatusConflict, "price_key_cadence_conflict", "default price key is held by a price on another cadence")
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

// priceConflict is ErrCatalogConflict naming the price (param: its document
// path) and, for a unique violation, the constraint it hit.
func priceConflict(err error, productKey, key string, w priceWrite) error {
	what := fmt.Sprintf("price %q of product %q", key, productKey)
	section := "prices"
	if w.providerOwned {
		section = "provider_prices"
		for psp, link := range w.links {
			what = fmt.Sprintf("provider price %q of product %q (%s)", key, productKey, providerObject(psp, link))
		}
	}
	if constraint, ok := db.UniqueViolation(err); ok && constraint != "" {
		what += " collides on " + constraint
	}
	return fmt.Errorf("%s: %w", what, ErrCatalogConflict.WithParam(fmt.Sprintf("products.%s.%s.%s", productKey, section, key)))
}

// providerObject names a binding's PSP and provider identifiers.
func providerObject(psp string, link map[string]string) string {
	parts := []string{"psp " + psp}
	refs := providerRefs(link)
	for _, k := range slices.Sorted(maps.Keys(refs)) {
		parts = append(parts, k+" "+refs[k])
	}
	return strings.Join(parts, ", ")
}
