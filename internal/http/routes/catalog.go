package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// page is a list route's paging parameters.
var page = []Param{text("cursor"), integer("limit")}

// Error codes of the catalog's nouns.
var (
	productErrors = []string{"catalog_benefit_overlap", "product_not_found", "product_tier_group_conflict", "product_tier_group_in_use", "resource_conflict", "revision_mismatch"}
	priceErrors   = []string{"catalog_benefit_overlap", "price_key_cadence_conflict", "price_key_not_found", "price_not_found", "product_not_found", "resource_conflict", "revision_mismatch", "trial_unsupported_on_rail"}
	meterErrors   = []string{
		"allowance_meter_not_found", "allowance_source_in_use", "allowance_source_invalid", "default_rate_card_not_found", "default_rate_card_required",
		"meter_in_use", "meter_rate_card_conflict", "rate_card_currency_mismatch", "rate_card_has_overrides", "rate_card_product_not_found",
		"revision_mismatch", "usage_meter_invalid", "usage_meter_not_found", "usage_rate_card_invalid",
	}
)

// catalogRoutes is what a merchant sells: products, prices, meters and their
// rate cards. The public product list, each with its current prices, is what a
// buyer may see; the merchant routes administer the catalog. A document
// applies as the apply manager and the edits as the edit manager: a document
// skips an object whose field an edit set differently.
var catalogRoutes = []Route{
	{Method: GET, Path: "/v1/catalog/products", Group: Checkout, Auth: AuthPublic,
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.Product]{}}}, Handler: h(handlers.ListPublicProducts)},

	{Method: GET, Path: "/v1/admin/catalog/revision", Group: Admin, Auth: AuthMerchant, Name: "GetCatalogRevision", Level: LevelRead,
		Responses: []Reply{{200, billing.CatalogRevision{}}}, Handler: h(handlers.GetCatalogRevision)},
	{Method: POST, Path: "/v1/admin/catalog/applications", Group: CatalogWrite, Auth: AuthMerchant, Name: "ApplyCatalog", Sensitive: true,
		Request: catalog.Application{}, Query: queryOf(handlers.CatalogApplicationQuery{}), Responses: []Reply{{200, billing.CatalogApplicationReceipt{}}}, Errors: codes(append([]string{"catalog_revision_conflict", "price_not_sellable"}, productErrors...)...), Handler: h(handlers.ApplyCatalog)},
	{Method: POST, Path: "/v1/admin/catalog/drift/refresh", Group: CatalogWrite, Auth: AuthMerchant, Name: "RefreshCatalogDrift", Sensitive: true,
		Responses: []Reply{{200, billing.CatalogDriftRefresh{}}}, Handler: h(handlers.RefreshCatalogDrift)},

	{Method: GET, Path: "/v1/admin/catalog/meters", Group: Admin, Auth: AuthMerchant, Name: "ListMeters", Level: LevelRead,
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.Meter]{}}}, Handler: h(handlers.ListMeters)},
	{Method: GET, Path: "/v1/admin/catalog/meters/{key}", Group: Admin, Auth: AuthMerchant, Name: "GetMeter", Level: LevelRead,
		Responses: []Reply{{200, billing.Meter{}}}, Errors: codes(meterErrors...), Handler: h(handlers.GetMeter)},
	{Method: PUT, Path: "/v1/admin/catalog/meters/{key}", Group: CatalogWrite, Auth: AuthMerchant, Name: "SetMeter", Sensitive: true,
		Request: billing.SetMeterParams{}, Responses: []Reply{{200, billing.Meter{}}}, Errors: codes(meterErrors...), Handler: h(handlers.SetMeter)},
	{Method: GET, Path: "/v1/admin/catalog/rate-overrides", Group: Admin, Auth: AuthMerchant, Name: "ListRateOverrides", Level: LevelRead,
		Query: params(page, text("customer_id"), text("meter_key")), Responses: []Reply{{200, billing.ListPage[billing.RateOverride]{}}}, Errors: codes("invalid_cursor", "invalid_query"), Handler: h(handlers.ListRateOverrides)},
	{Method: PUT, Path: "/v1/admin/catalog/rate-overrides/{customer_id}/{meter_key}", Group: CatalogWrite, Auth: AuthMerchant, Name: "SetRateOverride", Sensitive: true, Limit: middleware.AdminOperationGrant,
		Request: billing.SetRateOverrideParams{}, Responses: []Reply{{200, billing.RateOverride{}}}, Errors: codes(meterErrors...), Handler: h(handlers.SetRateOverride)},
	{Method: DELETE, Path: "/v1/admin/catalog/rate-overrides/{customer_id}/{meter_key}", Group: CatalogWrite, Auth: AuthMerchant, Name: "DeleteRateOverride", Sensitive: true, Limit: middleware.AdminOperationDestructive,
		Responses: []Reply{{204, nil}}, Errors: codes(meterErrors...), Handler: h(handlers.DeleteRateOverride)},

	{Method: POST, Path: "/v1/admin/catalog/product-archives", Group: CatalogWrite, Auth: AuthMerchant, Name: "ArchiveProduct", Sensitive: true, IdempotencyKey: true,
		Request: billing.ArchiveProductParams{}, Responses: []Reply{{200, billing.ProductArchive{}}}, Errors: codes(append([]string{"idempotency_key_required", "idempotency_key_reused", "invalid_param", "provider_cancel_held", "rebill_terms_committed", "service_unavailable"}, productErrors...)...), Handler: h(handlers.CreateProductArchive)},
	{Method: GET, Path: "/v1/admin/catalog/product-archives/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetProductArchive", Level: LevelRead,
		Responses: []Reply{{200, billing.ProductArchive{}}}, Errors: codes("invalid_param", "provider_cancel_held", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetProductArchive)},

	// The catalog assistant answers questions about the catalog and drafts
	// price changes for a person to review; it never changes a catalog row.
	{Method: POST, Path: "/v1/admin/catalog/ask", Group: Admin, Auth: AuthMerchant, Name: "AskCatalog", Level: LevelRead, When: FeatureCatalogCopilot,
		Request: billing.AskCatalogParams{}, Responses: []Reply{{200, billing.CatalogAnswer{}}}, Errors: codes("invalid_param", "model_unavailable", "rate_limit_exceeded", "service_unavailable"), Handler: h(handlers.AskCatalog)},

	{Method: POST, Path: "/v1/admin/catalog/products", Group: CatalogWrite, Auth: AuthMerchant, Name: "CreateProduct", Sensitive: true,
		Request: billing.CreateProductParams{}, Responses: []Reply{{201, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.CreateProduct)},
	{Method: GET, Path: "/v1/admin/catalog/products", Group: Admin, Auth: AuthMerchant, Name: "ListProducts", Level: LevelRead,
		Query: params(page, idsParam, queryOf(handlers.ProductListQuery{}), repeated("entitlement"), repeated("keys")), Responses: []Reply{{200, billing.ListPage[billing.Product]{}}}, Errors: codes(productErrors...), Handler: h(handlers.ListProducts)},
	{Method: GET, Path: "/v1/admin/catalog/products/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetProduct", Level: LevelRead,
		Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.GetProduct)},
	{Method: PATCH, Path: "/v1/admin/catalog/products/{id}", Group: CatalogWrite, Auth: AuthMerchant, Name: "UpdateProduct", Sensitive: true,
		Request: billing.UpdateProductParams{}, Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.UpdateProduct)},
	{Method: PUT, Path: "/v1/admin/catalog/products/by-key/{product_key}", Group: CatalogWrite, Auth: AuthMerchant, Name: "EnsureProduct", Sensitive: true,
		Request: billing.CreateProductParams{}, Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.EnsureProduct)},
	{Method: POST, Path: "/v1/admin/catalog/prices", Group: CatalogWrite, Auth: AuthMerchant, Name: "CreatePrice", Sensitive: true,
		Request: billing.CreatePriceParams{}, Responses: []Reply{{201, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.CreatePrice)},
	{Method: GET, Path: "/v1/admin/catalog/prices", Group: Admin, Auth: AuthMerchant, Name: "ListPrices", Level: LevelRead,
		Query: params(page, idsParam, queryOf(handlers.PriceListQuery{})), Responses: []Reply{{200, billing.ListPage[billing.Price]{}}}, Errors: codes(priceErrors...), Handler: h(handlers.ListPrices)},
	{Method: GET, Path: "/v1/admin/catalog/prices/{id}", Group: Admin, Auth: AuthMerchant, Name: "GetPrice", Level: LevelRead,
		Query: queryOf(handlers.PriceQuery{}), Responses: []Reply{{200, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.GetPrice)},
	{Method: GET, Path: "/v1/admin/catalog/prices/{id}/history", Group: Admin, Auth: AuthMerchant, Name: "ListPriceHistory", Level: LevelRead,
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.PriceKeyMovement]{}}}, Errors: codes(priceErrors...), Handler: h(handlers.ListPriceHistory)},
	{Method: PATCH, Path: "/v1/admin/catalog/prices/{id}", Group: CatalogWrite, Auth: AuthMerchant, Name: "UpdatePrice", Sensitive: true,
		Request: billing.UpdatePriceParams{}, Responses: []Reply{{200, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.UpdatePrice)},
}
