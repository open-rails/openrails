package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// merchantRoutes is the merchant itself: its configuration, outbound
// webhooks, portable archive and bulk import.
var merchantRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/configuration", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "GetMerchantConfiguration", NoConn: true,
		Responses: []Reply{{200, billing.MerchantConfigurationState{}}}, Handler: h(handlers.GetMerchantConfiguration)},
	// Merchant configuration changes only where Vault holds it: a file is
	// read-only.
	{Method: PATCH, Path: "/v1/merchant/configuration", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "UpdateMerchantConfiguration", When: FeatureMerchantConfigManages, NoConn: true,
		Request: billing.UpdateMerchantConfigurationParams{}, Responses: []Reply{{200, billing.MerchantConfigurationState{}}}, Errors: codes("card_vault_unknown", "charge_psp_incompatible", "charge_psp_unknown", "invalid_param", "platform_vault_mixed_class", "platform_vault_unavailable", "platform_vault_unreachable", "resource_conflict", "revision_mismatch", "service_unavailable"), Handler: h(handlers.UpdateMerchantConfiguration)},

	// Where the merchant's operational alerts are posted.
	{Method: GET, Path: "/v1/merchant/alert-webhooks", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "ListAlertWebhooks",
		Query: params(idsParam), Responses: []Reply{{200, billing.ListPage[billing.AlertWebhook]{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.ListAlertWebhooks)},
	{Method: POST, Path: "/v1/merchant/alert-webhooks", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "CreateAlertWebhook", When: FeatureMerchantConfigManages,
		Request: billing.CreateAlertWebhookParams{}, Responses: []Reply{{201, billing.AlertWebhook{}}}, Errors: codes("service_unavailable", "webhook_invalid"), Handler: h(handlers.CreateAlertWebhook)},
	{Method: DELETE, Path: "/v1/merchant/alert-webhooks/{id}", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "DeleteAlertWebhook", When: FeatureMerchantConfigManages,
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.DeleteAlertWebhook)},
	{Method: PATCH, Path: "/v1/merchant/alert-webhooks/{id}", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "UpdateAlertWebhook", When: FeatureMerchantConfigManages,
		Request: billing.UpdateAlertWebhookParams{}, Responses: []Reply{{200, billing.AlertWebhook{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable", "webhook_invalid"), Handler: h(handlers.UpdateAlertWebhook)},

	// The portable billing archive. Archives own their snapshot/restore
	// transaction and its merchant pin; no outer merchant connection is held
	// while transferring the artifact.
	{Method: GET, Path: "/v1/merchant/billing-archive", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "ExportBillingArchive", NoConn: true,
		Responses: []Reply{{200, Stream{"application/x-ndjson"}}}, Errors: codes("billing_archive_platform_cards", "billing_archive_unavailable", "billing_archive_unsupported_state"), Handler: h(handlers.ExportBillingArchive)},
	{Method: POST, Path: "/v1/merchant/billing-archive", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "ImportBillingArchive", NoConn: true,
		Request: Stream{"application/x-ndjson"}, Responses: []Reply{{200, billing.BillingArchiveImport{}}},
		Errors: codes("billing_archive_integrity", "billing_archive_invalid_artifact", "billing_archive_merchant_mismatch", "billing_archive_not_empty", "billing_archive_platform_cards", "billing_archive_unavailable", "billing_archive_unsupported_state", "request_body_too_large"), Handler: h(handlers.ImportBillingArchive)},
	// #737: the DeclaredBilling import door. A bulk book import rewrites
	// subscriptions, payments and payment methods wholesale, so it is the
	// merchant's configuration; the import pins its own merchant connection.
	{Method: POST, Path: "/v1/merchant/billing-import", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "ImportBilling", NoConn: true,
		Request: billingimport.DeclaredBilling{}, Responses: []Reply{{200, billingimport.Result{}}}, Errors: codes("as_of_required", "invalid_param", "invalid_psp_reference", "resource_conflict"), Handler: h(handlers.ImportDeclaredBilling)},
}
