package billing

const MaxEntitlementChecks = 100

// OfferKind distinguishes commercial access terms; currency preference never
// substitutes a subscription or rental for permanent ownership.
type OfferKind string

const (
	OfferPermanent OfferKind = "permanent"
	OfferFinite    OfferKind = "finite"
	OfferRecurring OfferKind = "recurring"
)

// OfferListParams applies to every requested key. Limit bounds each key's
// page; Cursors continues the keys it names from their NextCursor.
type OfferListParams struct {
	Kind              OfferKind
	PreferredCurrency string
	Limit             int
	Cursors           map[string]string
}

// OfferLookupRequest is the wire body of POST {catalog}/offers/lookup.
type OfferLookupRequest struct {
	Entitlements      []string          `json:"entitlements"`
	Kind              OfferKind         `json:"kind"`
	PreferredCurrency string            `json:"preferred_currency,omitempty"`
	PageSize          int               `json:"page_size,omitempty"`
	Cursors           map[string]string `json:"cursors,omitempty"`
}

// CatalogOffer is an active price and the product benefits it buys. The price
// ID is immutable; its key selects the current version when starting checkout.
type CatalogOffer struct {
	Kind                OfferKind       `json:"kind"`
	ProductID           string          `json:"product_id"`
	ProductKey          string          `json:"product_key"`
	ProductName         string          `json:"product_name"`
	PriceID             string          `json:"price_id"`
	PriceKey            string          `json:"price_key,omitempty"`
	UnitAmount          int64           `json:"unit_amount,string"`
	Currency            string          `json:"currency"`
	AccessDurationHours *int            `json:"access_duration_hours,omitempty"`
	AutoRenew           bool            `json:"auto_renew"`
	EntitlementsSpec    map[string]*int `json:"entitlements_spec"`
}

type OfferList struct {
	Data       []CatalogOffer `json:"data"`
	HasMore    bool           `json:"has_more"`
	NextCursor string         `json:"next_cursor,omitempty"`
}
