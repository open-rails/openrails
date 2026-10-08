package catalog

import (
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
)

// Extra-ness of remote Stripe objects vs the local catalog (#357/#358 phase D).
//
// A remote Stripe object is an EXTRA when the local catalog neither links it by
// ID nor identifies its exact local price (or product key) from metadata.
// This definition is shared by the internal/service extras report
// (DetectCatalogExtras) and the intent ledger's archive relevance checks
// (stripe_archive_product / stripe_archive_price): an archive intent stays
// applicable exactly while its object is STILL an extra — if the object has
// since been added/linked locally, archiving the remote copy would be wrong.

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
// OpenRails ownership marker (local price ID or older metadata/lookup key;
// "" = foreign).
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

// RemoteStripePriceKey reads the retained local price ID when available,
// otherwise the OpenRails metadata/lookup ownership marker. Older financial
// markers still identify owned extras, but cannot select a same-money local row.
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
