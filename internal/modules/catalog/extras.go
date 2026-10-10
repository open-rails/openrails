package catalog

import (
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
)

// A remote Stripe object is an extra when the local catalog neither links it by
// ID nor identifies its local price (or product key) from metadata. The extras
// report (DetectCatalogExtras) and the archive intents share this test: an
// archive intent applies only while its object is still an extra.

// ExtrasIndex is the local-catalog view the extra-ness predicates consult.
type ExtrasIndex struct {
	// StripeProductIDs / StripePriceIDs: remote ids some local price links.
	StripeProductIDs map[string]struct{}
	StripePriceIDs   map[string]struct{}
	// ProductKeys: local product content keys.
	ProductKeys map[string]struct{}
	// PriceIDs are immutable local identities, including archived prices.
	PriceIDs map[string]struct{}
}

// BuildExtrasIndex derives the index from the full local catalog rows.
func BuildExtrasIndex(products []*models.Product, prices []*models.Price) ExtrasIndex {
	ix := ExtrasIndex{
		StripeProductIDs: make(map[string]struct{}),
		StripePriceIDs:   make(map[string]struct{}),
		ProductKeys:      make(map[string]struct{}, len(products)),
		PriceIDs:         make(map[string]struct{}, len(prices)),
	}
	for _, p := range products {
		if key := strings.TrimSpace(p.Key); key != "" {
			ix.ProductKeys[key] = struct{}{}
		}
	}
	for _, pr := range prices {
		ix.PriceIDs[pr.ID.String()] = struct{}{}
		for _, stripe := range pr.PSPLinksForRail(models.RailStripe) {
			if id := strings.TrimSpace(stripe[models.RailKeyStripePriceID]); id != "" {
				ix.StripePriceIDs[id] = struct{}{}
			}
			if id := strings.TrimSpace(stripe[models.RailKeyStripeProductID]); id != "" {
				ix.StripeProductIDs[id] = struct{}{}
			}
		}
	}
	return ix
}

// StripeProductExtra reports whether the remote product is an extra, plus its
// OpenRails ownership marker (the openrails_product_key metadata = a product
// slug; "" = foreign).
func (ix ExtrasIndex) StripeProductExtra(sp StripeProduct) (isExtra bool, productKey string) {
	productKey = strings.TrimSpace(sp.Metadata[StripeMetadataOpenRailsProductKey])
	if _, linked := ix.StripeProductIDs[sp.ID]; linked {
		return false, productKey
	}
	if productKey != "" {
		if _, ok := ix.ProductKeys[productKey]; ok {
			return false, productKey
		}
	}
	return true, productKey
}

// StripePriceExtra reports whether the remote price is an extra, plus its
// OpenRails ownership marker (local price ID or metadata/lookup key; "" =
// foreign).
func (ix ExtrasIndex) StripePriceExtra(sp StripePrice) (isExtra bool, contentKey string) {
	contentKey = RemoteStripePriceKey(sp)
	if _, linked := ix.StripePriceIDs[sp.ID]; linked {
		return false, contentKey
	}
	if contentKey != "" {
		if _, ok := ix.PriceIDs[contentKey]; ok {
			return false, contentKey
		}
	}
	return true, contentKey
}

// RemoteStripePriceKey reads the retained local price ID, else the OpenRails
// metadata/lookup marker. A financial-terms marker identifies an owned extra
// but never selects a same-money local row.
func RemoteStripePriceKey(sp StripePrice) string {
	if id, err := uuid.Parse(strings.TrimSpace(sp.Metadata[StripeMetadataOpenRailsPriceID])); err == nil && id != uuid.Nil {
		return id.String()
	}
	if key := strings.TrimSpace(sp.Metadata[StripeMetadataOpenRailsPriceKey]); key != "" {
		return key
	}
	const prefix = "openrails."
	if key := strings.TrimSpace(sp.LookupKey); strings.HasPrefix(key, prefix) {
		return strings.TrimPrefix(key, prefix)
	}
	return ""
}
