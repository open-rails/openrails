package catalog

import (
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
)

// Extra-ness of remote Stripe objects vs the local catalog (#357/#358 phase D).
//
// A remote Stripe object is an EXTRA when the local catalog neither links it by
// id (a price row stores its stripe id) nor matches it by content key (the
// OpenRails ownership marker resolves to a local product key / price content
// key). This single definition is shared by the pkg/service extras report
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
	// PriceContentKeys: local price content keys
	// ("<product_key>.<currency>.<unit_amount>.<cycle>").
	PriceContentKeys map[string]struct{}
}

// BuildExtrasIndex derives the index from the full local catalog rows.
func BuildExtrasIndex(products []*models.Product, prices []*models.Price) ExtrasIndex {
	ix := ExtrasIndex{
		StripeProductIDs: make(map[string]struct{}),
		StripePriceIDs:   make(map[string]struct{}),
		ProductKeys:      make(map[string]struct{}, len(products)),
		PriceContentKeys: make(map[string]struct{}, len(prices)),
	}
	keyByProductID := make(map[string]string, len(products))
	for _, p := range products {
		if key := strings.TrimSpace(p.Key); key != "" {
			ix.ProductKeys[key] = struct{}{}
			keyByProductID[p.ID.String()] = key
		}
	}
	for _, pr := range prices {
		if key := keyByProductID[pr.ProductID.String()]; key != "" {
			ix.PriceContentKeys[OpenRailsPriceContentKey(key, pr.Currency, pr.Amount, pr.RecurringCycleDays())] = struct{}{}
		}
		if stripe := pr.PSPLinkForRail(models.RailStripe); stripe != nil {
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
// OpenRails ownership marker (the price content key from metadata/lookup_key;
// "" = foreign).
func (ix ExtrasIndex) StripePriceExtra(sp StripePrice) (isExtra bool, contentKey string) {
	contentKey = RemoteStripePriceContentKey(sp)
	if _, linked := ix.StripePriceIDs[sp.ID]; linked {
		return false, contentKey
	}
	if contentKey != "" {
		if _, ok := ix.PriceContentKeys[contentKey]; ok {
			return false, contentKey
		}
	}
	return true, contentKey
}

// RemoteStripePriceContentKey extracts the OpenRails financial content key
// ("<product_key>.<currency>.<unit_amount>.<cycle>") from a Stripe Price. It
// prefers the openrails_price_key metadata, falling back to deriving it from
// the lookup_key ("openrails.<content_key>"). "" = no marker (a Stripe-native
// price OpenRails doesn't own).
func RemoteStripePriceContentKey(sp StripePrice) string {
	if k := strings.TrimSpace(sp.Metadata[StripeMetadataOpenRailsPriceKey]); k != "" {
		return k
	}
	const lookupPrefix = "openrails."
	if lk := strings.TrimSpace(sp.LookupKey); strings.HasPrefix(lk, lookupPrefix) {
		return strings.TrimPrefix(lk, lookupPrefix)
	}
	return ""
}

// OpenRailsPriceContentKey is the content key derived from a price's financial
// substance — product key + immutable money terms. Format:
// "<product_key>.<currency>.<unit_amount>.<cycle>" where <cycle> is the
// provider day cadence or "onetime". Canonical here; pkg/service delegates.
func OpenRailsPriceContentKey(productKey, currency string, unitAmount int64, billingCycleDays *int) string {
	cycle := "onetime"
	if billingCycleDays != nil && *billingCycleDays > 0 {
		cycle = strconv.Itoa(*billingCycleDays)
	}
	return strings.Join([]string{
		strings.TrimSpace(productKey),
		strings.ToLower(strings.TrimSpace(currency)),
		strconv.FormatInt(unitAmount, 10),
		cycle,
	}, ".")
}
