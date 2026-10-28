package billing

import "time"

// ProviderCatalogKind is what a provider catalog object is. An NMI plan is a
// price.
type ProviderCatalogKind string

const (
	ProviderCatalogProduct ProviderCatalogKind = "product"
	ProviderCatalogPrice   ProviderCatalogKind = "price"
)

// ProviderCatalogObject is one object of a PSP's own catalog (a Stripe
// product or price, an NMI plan) as OpenRails last observed it. Retired: the
// provider no longer sells it (inactive, deleted, or its product is); a
// provider price mapping it is not for sale but keeps billing. Price is the
// live provider price mapping it; null is unmapped. Subscriptions counts the
// import's staged subscriptions naming it, when the list names an import.
type ProviderCatalogObject struct {
	Kind          ProviderCatalogKind   `json:"kind"`
	Ref           string                `json:"ref"`
	ProductRef    *string               `json:"product_ref"`
	Name          *string               `json:"name"`
	Active        bool                  `json:"active"`
	DeletedAt     *time.Time            `json:"deleted_at"`
	Retired       bool                  `json:"retired"`
	Terms         *ProviderCatalogTerms `json:"terms"`
	ObservedAt    time.Time             `json:"observed_at"`
	ObservedVia   string                `json:"observed_via"`
	Price         *PriceID              `json:"price"`
	Subscriptions *int                  `json:"subscriptions"`
}

// ProviderCatalogTerms are a provider price's terms as the provider states
// them. StatedAmount is a decimal in major units ("9.99"), null when the
// price has no fixed amount; Currency is null where the provider names none
// (NMI). At most one billing interval is set.
type ProviderCatalogTerms struct {
	StatedAmount          *string `json:"stated_amount"`
	Currency              *string `json:"currency"`
	BillingIntervalHours  *int    `json:"billing_interval_hours"`
	BillingIntervalMonths *int    `json:"billing_interval_months"`
}

// ProviderCatalogListParams filters one PSP's observed catalog, in (kind,
// ref) order. Mapped and Retired filter when set. Import lists only the
// prices that import's staged subscriptions name: its plans to map.
type ProviderCatalogListParams struct {
	PageRequest
	PSP     PSPID
	Kind    ProviderCatalogKind
	Mapped  *bool
	Retired *bool
	Import  *PSPImportID
}
