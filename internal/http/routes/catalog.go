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
	productErrors = []string{"product_not_found", "product_tier_group_conflict", "product_tier_group_in_use", "resource_conflict"}
	priceErrors   = []string{"price_key_cadence_conflict", "price_key_not_found", "price_not_found", "product_not_found", "resource_conflict", "trial_unsupported_on_rail"}
	meterErrors   = []string{
		"allowance_meter_not_found", "allowance_source_in_use", "allowance_source_invalid", "default_rate_card_not_found", "default_rate_card_required",
		"meter_in_use", "meter_rate_card_conflict", "rate_card_currency_mismatch", "rate_card_has_overrides", "rate_card_product_not_found",
		"usage_meter_invalid", "usage_meter_not_found", "usage_rate_card_invalid",
	}
)

// catalogRoutes is what a merchant sells: products, prices, meters and their
// rate cards. The public pair lists what a buyer may see; the merchant routes
// administer the catalog. A write is mounted only where the deployment allows
// catalog updates.
var catalogRoutes = []Route{
	{Method: GET, Path: "/v1/products", Group: Checkout, Auth: AuthPublic,
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.Product]{}}}, Handler: h(handlers.ListPublicProducts)},
	{Method: GET, Path: "/v1/prices", Group: Checkout, Auth: AuthPublic,
		Query: params(page, queryOf(handlers.PublicPriceListQuery{})), Responses: []Reply{{200, billing.ListPage[billing.Price]{}}}, Handler: h(handlers.ListPublicPrices)},

	{Method: GET, Path: "/v1/merchant/catalog/revision", Group: Merchant, Auth: AuthMerchant, Name: "GetCatalogRevision", Level: LevelRead, Resources: res(ResCatalog),
		Responses: []Reply{{200, billing.CatalogRevision{}}}, Handler: h(handlers.GetCatalogRevision)},
	{Method: POST, Path: "/v1/merchant/catalog/applications", Group: MerchantConfig, Auth: AuthMerchant, Name: "ApplyCatalog", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Request: catalog.Application{}, Responses: []Reply{{200, billing.CatalogApplicationReceipt{}}}, Errors: codes(append([]string{"catalog_revision_conflict", "price_not_sellable"}, productErrors...)...), Handler: h(handlers.ApplyCatalog)},
	{Method: POST, Path: "/v1/merchant/catalog/entitlement-replacements", Group: MerchantConfig, Auth: AuthMerchant, Name: "ReplaceEntitlements", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Request: billing.ReplaceEntitlementsParams{}, Responses: []Reply{{200, billing.CatalogApplicationReceipt{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ReplaceEntitlements)},
	{Method: GET, Path: "/v1/merchant/catalog/drift", Group: Merchant, Auth: AuthMerchant, Name: "ListCatalogDrift", Level: LevelRead, Resources: res(ResCatalog),
		Query: params(page, queryOf(handlers.CatalogDriftQuery{})), Responses: []Reply{{200, billing.ListPage[billing.CatalogDrift]{}}}, Handler: h(handlers.ListCatalogDrift)},
	{Method: POST, Path: "/v1/merchant/catalog/drift/refresh", Group: MerchantConfig, Auth: AuthMerchant, Name: "RefreshCatalogDrift", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Responses: []Reply{{200, billing.CatalogDriftRefresh{}}}, Handler: h(handlers.RefreshCatalogDrift)},

	{Method: GET, Path: "/v1/merchant/catalog/meters", Group: Merchant, Auth: AuthMerchant, Name: "ListMeters", Level: LevelRead, Resources: res(ResCatalog),
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.Meter]{}}}, Handler: h(handlers.ListMeters)},
	{Method: GET, Path: "/v1/merchant/catalog/meters/{key}", Group: Merchant, Auth: AuthMerchant, Name: "GetMeter", Level: LevelRead, Resources: res(ResCatalog),
		Responses: []Reply{{200, billing.Meter{}}}, Errors: codes(meterErrors...), Handler: h(handlers.GetMeter)},
	{Method: PUT, Path: "/v1/merchant/catalog/meters/{key}", Group: MerchantConfig, Auth: AuthMerchant, Name: "SetMeter", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Request: billing.SetMeterParams{}, Responses: []Reply{{200, billing.Meter{}}}, Errors: codes(meterErrors...), Handler: h(handlers.SetMeter)},
	{Method: PUT, Path: "/v1/merchant/catalog/meters/{key}/rate-card", Group: MerchantConfig, Auth: AuthMerchant, Name: "SetMeterRateCard", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Request: billing.SetMeterRateCardParams{}, Responses: []Reply{{200, billing.Meter{}}}, Errors: codes(meterErrors...), Handler: h(handlers.SetMeterRateCard)},
	{Method: DELETE, Path: "/v1/merchant/catalog/meters/{key}/rate-card", Group: MerchantConfig, Auth: AuthMerchant, Name: "DeleteMeterRateCard", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Responses: []Reply{{204, nil}}, Errors: codes(meterErrors...), Handler: h(handlers.DeleteMeterRateCard)},
	{Method: GET, Path: "/v1/merchant/catalog/meters/{key}/rate-overrides", Group: Merchant, Auth: AuthMerchant, Name: "ListMeterRateOverrides", Level: LevelRead, Resources: res(ResCatalog),
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.RateOverride]{}}}, Errors: codes(meterErrors...), Handler: h(handlers.ListMeterRateOverrides)},
	{Method: GET, Path: "/v1/merchant/customers/{customer_id}/rate-overrides", Group: Merchant, Auth: AuthMerchant, Name: "ListRateOverrides", Level: LevelRead, Resources: res(ResCatalog),
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.RateOverride]{}}}, Handler: h(handlers.ListRateOverrides)},
	{Method: PUT, Path: "/v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}", Group: MerchantConfig, Auth: AuthMerchant, Name: "SetRateOverride", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, Limit: middleware.AdminOperationGrant, CatalogWrite: true,
		Request: billing.SetRateOverrideParams{}, Responses: []Reply{{200, billing.RateOverride{}}}, Errors: codes(meterErrors...), Handler: h(handlers.SetRateOverride)},
	{Method: DELETE, Path: "/v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}", Group: MerchantConfig, Auth: AuthMerchant, Name: "DeleteRateOverride", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, Limit: middleware.AdminOperationDestructive, CatalogWrite: true,
		Responses: []Reply{{204, nil}}, Errors: codes(meterErrors...), Handler: h(handlers.DeleteRateOverride)},

	{Method: POST, Path: "/v1/merchant/catalog/product-archives", Group: MerchantConfig, Auth: AuthMerchant, Name: "ArchiveProduct", Level: LevelAdmin, Resources: res(ResCatalog, ResRefunds), Sensitive: true, CatalogWrite: true, IdempotencyKey: true,
		Request: billing.ArchiveProductParams{}, Responses: []Reply{{200, billing.ProductArchive{}}}, Errors: codes(append([]string{"idempotency_key_required", "idempotency_key_reused", "invalid_param", "provider_cancel_held", "rebill_terms_committed", "service_unavailable"}, productErrors...)...), Handler: h(handlers.CreateProductArchive)},
	{Method: GET, Path: "/v1/merchant/catalog/product-archives/{id}", Group: Merchant, Auth: AuthMerchant, Name: "GetProductArchive", Level: LevelRead, Resources: res(ResCatalog, ResPayments),
		Responses: []Reply{{200, billing.ProductArchive{}}}, Errors: codes("invalid_param", "provider_cancel_held", "rebill_terms_committed", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetProductArchive)},

	// The catalog assistant answers questions about the catalog and drafts
	// price changes for a person to review; it never changes a catalog row.
	{Method: POST, Path: "/v1/merchant/catalog/ask", Group: Merchant, Auth: AuthMerchant, Name: "AskCatalog", Level: LevelRead, Resources: res(ResCatalog), When: FeatureCatalogCopilot,
		Request: billing.AskCatalogParams{}, Responses: []Reply{{200, billing.CatalogAnswer{}}}, Errors: codes("invalid_param", "model_unavailable", "rate_limit_exceeded", "service_unavailable"), Handler: h(handlers.AskCatalog)},

	{Method: POST, Path: "/v1/merchant/catalog/products", Group: MerchantConfig, Auth: AuthMerchant, Name: "CreateProduct", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Request: billing.CreateProductParams{}, Responses: []Reply{{201, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.CreateProduct)},
	{Method: GET, Path: "/v1/merchant/catalog/products", Group: Merchant, Auth: AuthMerchant, Name: "ListProducts", Level: LevelRead, Resources: res(ResCatalog),
		Query: params(page, queryOf(handlers.ProductListQuery{})), Responses: []Reply{{200, billing.ListPage[billing.Product]{}}}, Errors: codes(productErrors...), Handler: h(handlers.ListProducts)},
	{Method: GET, Path: "/v1/merchant/catalog/products/{id}", Group: Merchant, Auth: AuthMerchant, Name: "GetProduct", Level: LevelRead, Resources: res(ResCatalog),
		Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.GetProduct)},
	{Method: PATCH, Path: "/v1/merchant/catalog/products/{id}", Group: MerchantConfig, Auth: AuthMerchant, Name: "UpdateProduct", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Request: billing.UpdateProductParams{}, Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.UpdateProduct)},
	{Method: GET, Path: "/v1/merchant/catalog/products/by-key/{product_key}", Group: Merchant, Auth: AuthMerchant, Name: "GetProductByKey", Level: LevelRead, Resources: res(ResCatalog),
		Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.GetProductByKey)},
	{Method: PUT, Path: "/v1/merchant/catalog/products/by-key/{product_key}", Group: MerchantConfig, Auth: AuthMerchant, Name: "EnsureProduct", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Request: billing.CreateProductParams{}, Responses: []Reply{{200, billing.Product{}}}, Errors: codes(productErrors...), Handler: h(handlers.EnsureProduct)},
	{Method: POST, Path: "/v1/merchant/catalog/prices", Group: MerchantConfig, Auth: AuthMerchant, Name: "CreatePrice", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Request: billing.CreatePriceParams{}, Responses: []Reply{{201, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.CreatePrice)},
	{Method: GET, Path: "/v1/merchant/catalog/prices", Group: Merchant, Auth: AuthMerchant, Name: "ListPrices", Level: LevelRead, Resources: res(ResCatalog),
		Query: params(page, queryOf(handlers.PriceListQuery{})), Responses: []Reply{{200, billing.ListPage[billing.Price]{}}}, Errors: codes(priceErrors...), Handler: h(handlers.ListPrices)},
	{Method: GET, Path: "/v1/merchant/catalog/prices/{id}", Group: Merchant, Auth: AuthMerchant, Name: "GetPrice", Level: LevelRead, Resources: res(ResCatalog),
		Query: queryOf(handlers.PriceQuery{}), Responses: []Reply{{200, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.GetPrice)},
	{Method: PATCH, Path: "/v1/merchant/catalog/prices/{id}", Group: MerchantConfig, Auth: AuthMerchant, Name: "UpdatePrice", Level: LevelAdmin, Resources: res(ResCatalog), Sensitive: true, CatalogWrite: true,
		Request: billing.UpdatePriceParams{}, Responses: []Reply{{200, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.UpdatePrice)},
	{Method: GET, Path: "/v1/merchant/catalog/products/by-key/{product_key}/prices/by-key/{key}", Group: Merchant, Auth: AuthMerchant, Name: "GetPriceByKey", Level: LevelRead, Resources: res(ResCatalog),
		Responses: []Reply{{200, billing.Price{}}}, Errors: codes(priceErrors...), Handler: h(handlers.GetPriceByKey)},
	{Method: GET, Path: "/v1/merchant/catalog/products/by-key/{product_key}/prices/by-key/{key}/history", Group: Merchant, Auth: AuthMerchant, Name: "ListPriceKeyHistory", Level: LevelRead, Resources: res(ResCatalog),
		Query: page, Responses: []Reply{{200, billing.ListPage[billing.PriceKeyMovement]{}}}, Errors: codes(priceErrors...), Handler: h(handlers.ListPriceKeyHistory)},
	{Method: POST, Path: "/v1/merchant/catalog/offers/lookup", Group: Merchant, Auth: AuthMerchant, Name: "ListOffers", Level: LevelRead, Resources: res(ResCatalog),
		Request: billing.OfferListParams{}, Responses: []Reply{{200, billing.OfferPages{}}}, Handler: h(handlers.ListOffers)},
}
