package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// Error codes of the catalog's nouns.
var (
	productErrors = []billing.ErrorCode{"catalog_benefit_overlap", "product_not_found", "product_tier_group_conflict", "product_tier_group_in_use", "resource_conflict", "revision_mismatch"}
	priceErrors   = []billing.ErrorCode{"catalog_benefit_overlap", "price_key_cadence_conflict", "price_key_not_found", "price_not_found", "price_not_sellable", "product_not_found", "resource_conflict", "revision_mismatch", "trial_unsupported_on_rail"}
	meterErrors   = []billing.ErrorCode{
		"default_rate_card_not_found", "meter_in_use", "meter_rate_card_conflict", "rate_card_product_not_found",
		"revision_mismatch", "usage_meter_invalid", "usage_meter_not_found", "usage_rate_card_invalid",
	}
)

// catalogRoutes is what a merchant sells: products, prices, meters and their
// rate cards. The host's backend reads what is on sale and puts it in its own
// pages (billing.BuyerPrices); the merchant routes administer the catalog. A document applies as the
// apply manager and the edits as the edit manager: a document skips an
// object whose field an edit set differently.
var catalogRoutes = []Route{
	{Method: POST, Path: "/v1/catalog/apply", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ApplyCatalog", Level: LevelUpdate,
		Doc: "ApplyCatalog applies a catalog document as the apply manager. Each product, price key and meter in " +
			"it applies whole, unless a field it names was set differently by an edit: that object is skipped and " +
			"listed in the receipt's Conflicts, the rest applies, and the document is not recorded as applied, so " +
			"applying it again retries the skipped objects. params.Force overwrites those fields instead. A " +
			"document that applied whole is remembered by its canonical content hash: applying it again returns " +
			"the original receipt even after later catalog edits. It is an operator action, never a side effect " +
			"of starting: run it from a command (openrails catalog apply, or the host's own) or the console.",
		Request: &catalog.Application{}, Query: billing.ApplyCatalogParams{}, Responses: []Reply{{200, billing.CatalogApplicationReceipt{}}}, Errors: codes(append([]billing.ErrorCode{"catalog_revision_conflict", "merchant_unit_unsupported", "price_not_sellable"}, productErrors...)...), Handler: h(handlers.ApplyCatalog)},

	{Method: GET, Path: "/v1/catalog/meters", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListMeters", Level: LevelRead,
		Cache: config.ReadMeters,
		Doc:   "ListMeters returns one page of the merchant's meters, by key.",
		Query: billing.PageRequest{}, Responses: []Reply{{200, billing.ListPage[billing.Meter]{}}}, Handler: h(handlers.ListMeters)},
	{Method: GET, Path: "/v1/catalog/meters/{key}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "GetMeter", Level: LevelRead,
		Args:      []any{""},
		Cache:     config.ReadMeters,
		Doc:       "GetMeter reads one meter with its rate card.",
		Responses: []Reply{{200, billing.Meter{}}}, Errors: codes(meterErrors...), Handler: h(handlers.GetMeter)},
	{Method: PUT, Path: "/v1/catalog/meters/{key}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "SetMeter", Level: LevelUpdate,
		Args: []any{""},
		Doc: "SetMeter declares the meter under key, creating it or changing its definition, and with " +
			"params.RateCard sets or (null) removes the rate card that prices its usage. A meter with recorded " +
			"usage keeps its definition (billing.ErrConflict, code meter_in_use).",
		Request: billing.SetMeterParams{}, Responses: []Reply{{200, billing.Meter{}}}, Errors: codes(meterErrors...), Handler: h(handlers.SetMeter)},

	// The catalog assistant answers questions about the catalog and drafts
	// price changes for a person to review; it never changes a catalog row.
	{Method: POST, Path: "/v1/console/catalog/ask", Group: CatalogAdmin, Auth: AuthMerchant, ConsoleOnly: true, Level: LevelRead, When: FeatureCatalogCopilot,
		Request: billing.AskCatalogParams{}, Responses: []Reply{{200, billing.CatalogAnswer{}}}, Errors: codes("invalid_param", "model_unavailable", "rate_limit_exceeded", "service_unavailable"), Handler: h(handlers.AskCatalog)},

	{Method: POST, Path: "/v1/catalog/products", Group: CatalogAdmin, Auth: AuthMerchant, Name: "CreateProduct", Level: LevelUpdate,
		Doc:     "CreateProduct creates a product.",
		Request: billing.CreateProductParams{}, Responses: []Reply{{201, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.CreateProduct)},
	{Method: GET, Path: "/v1/catalog/products", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListProducts", Level: LevelRead,
		Cache:  config.ReadCatalog,
		Client: HandWritten,
		Doc: "ListProducts returns one page of products, newest first, each with its current prices. params.Keys " +
			"reads products by key.",
		Query: billing.ProductListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Product]{}}}, Errors: codes(productErrors...), Handler: h(handlers.ListProducts)},
	{Method: GET, Path: "/v1/catalog/products/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "GetProduct", Level: LevelRead,
		Args:      []any{billing.ProductID{}},
		Cache:     config.ReadCatalog,
		Doc:       "GetProduct reads a product with its current prices.",
		Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.GetProduct)},
	{Method: PATCH, Path: "/v1/catalog/products/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "UpdateProduct", Level: LevelUpdate,
		Args:    []any{billing.ProductID{}},
		Doc:     "UpdateProduct changes a product's fields; omitted ones keep their values.",
		Request: billing.UpdateProductParams{}, Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.UpdateProduct)},
	{Method: DELETE, Path: "/v1/catalog/products/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "DeleteProduct", Level: LevelUpdate,
		Args: []any{billing.ProductID{}},
		Doc: "DeleteProduct deletes a product nothing ever referred to, with its prices: reads leave it out, and " +
			"creating its key again revives it. A product a purchase, subscription, grant, access, purchase, " +
			"migration, provider write, PSP link or rate card refers to is refused (billing.ErrConflict, code " +
			"product_in_use): archive it with UpdateProduct instead. A stale ExpectedRevision is refused with " +
			"revision_mismatch.",
		Query: billing.DeleteProductParams{}, Responses: []Reply{{204, nil}}, Errors: codes(billing.CodeProductInUse, "product_not_found", "revision_mismatch"), Handler: h(handlers.DeleteProduct)},
	{Method: POST, Path: "/v1/catalog/prices", Group: CatalogAdmin, Auth: AuthMerchant, Name: "CreatePrice", Level: LevelUpdate,
		Doc: "CreatePrice creates a price on the product named by exactly one of ProductID, ProductKey and " +
			"ProductData. A price's terms never change; selling on other terms means another price.",
		Request: billing.CreatePriceParams{}, Responses: []Reply{{201, billing.Price{}}}, Errors: codes(append([]billing.ErrorCode{"merchant_unit_unsupported"}, priceErrors...)...), Handler: h(handlers.CreatePrice)},
	{Method: POST, Path: "/v1/catalog/provider-prices", Group: CatalogAdmin, Auth: AuthMerchant, Name: "CreateProviderPrice", Level: LevelUpdate,
		Doc: "CreateProviderPrice links a price a provider owns and runs to a product (a legacy NMI plan, a Stripe " +
			"price, a CCBill FlexForm). Its terms are read from the provider, declared for CCBill; nothing is " +
			"written to the provider.",
		Request: billing.CreateProviderPriceParams{}, Responses: []Reply{{201, billing.Price{}}}, Errors: codes(append([]billing.ErrorCode{"provider_price_unreadable"}, priceErrors...)...), Handler: h(handlers.CreateProviderPrice)},
	{Method: GET, Path: "/v1/catalog/prices", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListPrices", Level: LevelRead,
		Cache: config.ReadCatalog,
		Doc: "ListPrices returns one page of prices, newest first. params.ProductKey and params.Key read prices by " +
			"key: a key's current price is its one not archived.",
		Query: billing.PriceListParams{}, Responses: []Reply{{200, billing.ListPage[billing.Price]{}}}, Errors: codes(priceErrors...), Handler: h(handlers.ListPrices)},
	{Method: GET, Path: "/v1/catalog/prices/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "GetPrice", Level: LevelRead,
		Args:      []any{billing.PriceID("")},
		Cache:     config.ReadCatalog,
		Doc:       "GetPrice reads a price.",
		Responses: []Reply{{200, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.GetPrice)},
	{Method: GET, Path: "/v1/catalog/prices/{id}/verification", Group: CatalogAdmin, Auth: AuthMerchant, Name: "VerifyPrice", Level: LevelRead,
		Args: []any{billing.PriceID("")},
		Doc: "VerifyPrice reads a price and, for a provider price, its terms at its provider: Provider.SyncStatus " +
			"and Drift say whether they still match. It never writes, and is never answered from memory.",
		Responses: []Reply{{200, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.VerifyPrice)},
	{Method: GET, Path: "/v1/catalog/prices/{id}/history", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListPriceHistory", Level: LevelRead,
		Args: []any{billing.PriceID("")},
		Doc: "ListPriceHistory returns one page of the history of a price's key, most recent first: when the key " +
			"moved to which price, or was retired.",
		Query: billing.PageRequest{}, Responses: []Reply{{200, billing.ListPage[billing.PriceKeyMovement]{}}}, Errors: codes(priceErrors...), Handler: h(handlers.ListPriceHistory)},
	{Method: PATCH, Path: "/v1/catalog/prices/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "UpdatePrice", Level: LevelUpdate,
		Args: []any{billing.PriceID("")},
		Doc: "UpdatePrice archives or restores a price, or changes its PSP links. Omitted fields keep their " +
			"values; the key and financial terms are immutable.",
		Request: billing.UpdatePriceParams{}, Responses: []Reply{{200, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.UpdatePrice)},
}
