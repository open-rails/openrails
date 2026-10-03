package billing

type ProductListParams struct {
	PageOptions
	CatalogID string
	Archived  *bool
	TierGroup string
}

type PriceListParams struct {
	PageOptions
	CatalogID string
	ProductID string
	Archived  *bool
	Currency  string
	Type      string
}
