package billing

// MaxEntitlementChecks bounds the entitlement keys of one check or offer
// lookup.
const MaxEntitlementChecks = 100

// OfferKind distinguishes commercial access terms; currency preference never
// substitutes a subscription or rental for permanent ownership.
type OfferKind string

const (
	// OfferPermanent is a one-time purchase that grants access for good.
	OfferPermanent OfferKind = "permanent"
	// OfferFinite is a one-time purchase that grants access for a period.
	OfferFinite OfferKind = "finite"
	// OfferRecurring is a subscription.
	OfferRecurring OfferKind = "recurring"
)

// OfferListParams looks up the live offers that grant each of Entitlements
// (at most MaxEntitlementChecks), Limit offers per entitlement, offers in
// PreferredCurrency first. Cursors continues the entitlements it names from
// a previous page's next cursor.
type OfferListParams struct {
	Entitlements      []string          `json:"entitlements"`
	Kind              OfferKind         `json:"kind"`
	PreferredCurrency string            `json:"preferred_currency,omitempty"`
	Limit             int               `json:"limit,omitempty"`
	Cursors           map[string]string `json:"cursors,omitempty"`
}

// Offer is a live price and the product it buys. Its fields are the
// product's and the price's: the price's ID is immutable, its key selects its
// current version at checkout.
type Offer struct {
	Kind                OfferKind       `json:"kind"`
	ProductID           ProductID       `json:"product_id"`
	ProductKey          string          `json:"product_key"`
	ProductDisplayName  string          `json:"product_display_name"`
	EntitlementsSpec    map[string]*int `json:"entitlements_spec"`
	PriceID             PriceID         `json:"price_id"`
	PriceKey            string          `json:"price_key"`
	UnitAmount          int64           `json:"unit_amount,string"`
	Currency            string          `json:"currency"`
	AccessDurationHours *int            `json:"access_duration_hours"`
	AutoRenew           bool            `json:"auto_renew"`
}

// OfferPages is one page of offers per requested entitlement.
type OfferPages map[string]ListPage[Offer]
