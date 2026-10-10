package billing

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
