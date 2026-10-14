package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// page is a list route's paging parameters.
var page = []Param{text("cursor"), integer("limit")}

// Error codes of the catalog's nouns.
var (
	productErrors = []string{"catalog_benefit_overlap", "product_not_found", "product_tier_group_conflict", "product_tier_group_in_use", "resource_conflict", "revision_mismatch"}
	priceErrors   = []string{"catalog_benefit_overlap", "price_key_cadence_conflict", "price_key_not_found", "price_not_found", "price_not_sellable", "product_not_found", "resource_conflict", "revision_mismatch", "trial_unsupported_on_rail"}
	meterErrors   = []string{
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
		Request: catalog.Application{}, Query: queryOf(handlers.CatalogApplicationQuery{}), Responses: []Reply{{200, billing.CatalogApplicationReceipt{}}}, Errors: codes(append([]string{"catalog_revision_conflict", "price_not_sellable"}, productErrors...)...), Handler: h(handlers.ApplyCatalog)},

	{Method: GET, Path: "/v1/catalog/meters", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListMeters", Level: LevelRead,
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.Meter]{}}}, Handler: h(handlers.ListMeters)},
	{Method: GET, Path: "/v1/catalog/meters/{key}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "GetMeter", Level: LevelRead,
		Responses: []Reply{{200, billing.Meter{}}}, Errors: codes(meterErrors...), Handler: h(handlers.GetMeter)},
	{Method: PUT, Path: "/v1/catalog/meters/{key}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "SetMeter", Level: LevelUpdate,
		Request: billing.SetMeterParams{}, Responses: []Reply{{200, billing.Meter{}}}, Errors: codes(meterErrors...), Handler: h(handlers.SetMeter)},

	// The catalog assistant answers questions about the catalog and drafts
	// price changes for a person to review; it never changes a catalog row.
	{Method: POST, Path: "/v1/console/catalog/ask", Group: CatalogAdmin, Auth: AuthMerchant, ConsoleOnly: true, Level: LevelRead, When: FeatureCatalogCopilot,
		Request: billing.AskCatalogParams{}, Responses: []Reply{{200, billing.CatalogAnswer{}}}, Errors: codes("invalid_param", "model_unavailable", "rate_limit_exceeded", "service_unavailable"), Handler: h(handlers.AskCatalog)},

	{Method: POST, Path: "/v1/catalog/products", Group: CatalogAdmin, Auth: AuthMerchant, Name: "CreateProduct", Level: LevelUpdate,
		Request: billing.CreateProductParams{}, Responses: []Reply{{201, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.CreateProduct)},
	{Method: GET, Path: "/v1/catalog/products", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListProducts", Level: LevelRead,
		Query: params(page, idsParam, queryOf(handlers.ProductListQuery{}), repeated("entitlement"), repeated("keys")), Responses: []Reply{{200, billing.ListPage[billing.Product]{}}}, Errors: codes(productErrors...), Handler: h(handlers.ListProducts)},
	{Method: GET, Path: "/v1/catalog/products/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "GetProduct", Level: LevelRead,
		Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.GetProduct)},
	{Method: PATCH, Path: "/v1/catalog/products/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "UpdateProduct", Level: LevelUpdate,
		Request: billing.UpdateProductParams{}, Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.UpdateProduct)},
	{Method: DELETE, Path: "/v1/catalog/products/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "DeleteProduct", Level: LevelUpdate,
		Responses: []Reply{{204, nil}}, Errors: codes(billing.CodeProductInUse, "product_not_found"), Handler: h(handlers.DeleteProduct)},
	{Method: POST, Path: "/v1/catalog/prices", Group: CatalogAdmin, Auth: AuthMerchant, Name: "CreatePrice", Level: LevelUpdate,
		Request: billing.CreatePriceParams{}, Responses: []Reply{{201, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.CreatePrice)},
	{Method: POST, Path: "/v1/catalog/provider-prices", Group: CatalogAdmin, Auth: AuthMerchant, Name: "CreateProviderPrice", Level: LevelUpdate,
		Request: billing.CreateProviderPriceParams{}, Responses: []Reply{{201, billing.Price{}}}, Errors: codes(append([]string{"provider_price_unreadable"}, priceErrors...)...), Handler: h(handlers.CreateProviderPrice)},
	{Method: GET, Path: "/v1/catalog/prices", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListPrices", Level: LevelRead,
		Query: params(page, idsParam, queryOf(handlers.PriceListQuery{})), Responses: []Reply{{200, billing.ListPage[billing.Price]{}}}, Errors: codes(priceErrors...), Handler: h(handlers.ListPrices)},
	{Method: GET, Path: "/v1/catalog/prices/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "GetPrice", Level: LevelRead,
		Query: queryOf(handlers.PriceQuery{}), Responses: []Reply{{200, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.GetPrice)},
	{Method: GET, Path: "/v1/catalog/prices/{id}/history", Group: CatalogAdmin, Auth: AuthMerchant, Name: "ListPriceHistory", Level: LevelRead,
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.PriceKeyMovement]{}}}, Errors: codes(priceErrors...), Handler: h(handlers.ListPriceHistory)},
	{Method: PATCH, Path: "/v1/catalog/prices/{id}", Group: CatalogAdmin, Auth: AuthMerchant, Name: "UpdatePrice", Level: LevelUpdate,
		Request: billing.UpdatePriceParams{}, Responses: []Reply{{200, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.UpdatePrice)},
}
