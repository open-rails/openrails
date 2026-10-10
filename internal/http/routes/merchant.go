package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// merchantRoutes is the merchant itself: its configuration, API host,
// outbound webhooks, portable archive and bulk import.
var merchantRoutes = []Route{
	{Method: GET, Path: "/v1/admin/configuration", Group: MerchantConfig, Auth: AuthMerchant, Name: "GetMerchantConfiguration", NoConn: true,
		Responses: []Reply{{200, billing.MerchantConfigurationState{}}}, Handler: h(handlers.GetMerchantConfiguration)},
	// Merchant configuration changes only where Vault holds it: a file is
	// read-only.
	{Method: PATCH, Path: "/v1/admin/configuration", Group: MerchantConfig, Auth: AuthMerchant, Name: "UpdateMerchantConfiguration", Sensitive: true, When: FeatureMerchantConfigEdits, NoConn: true,
		Request: billing.UpdateMerchantConfigurationParams{}, Responses: []Reply{{200, billing.MerchantConfigurationState{}}}, Errors: codes("invalid_param", "revision_mismatch", "service_unavailable"), Handler: h(handlers.UpdateMerchantConfiguration)},

	// The host the merchant's public routes resolve from (#734).
	{Method: GET, Path: "/v1/admin/api-host", Group: MerchantConfig, Auth: AuthMerchant, Name: "GetAPIHost", When: FeatureMerchantDirectory, NoConn: true,
		Responses: []Reply{{200, billing.MerchantAPIHost{}}}, Errors: codes("merchant_unresolved", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetMerchantAPIHost)},

	// Where the merchant's operational alerts are posted.
	{Method: GET, Path: "/v1/admin/alert-webhooks", Group: MerchantConfig, Auth: AuthMerchant, Name: "ListAlertWebhooks",
		Query: params(idsParam), Responses: []Reply{{200, billing.ListPage[billing.AlertWebhook]{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.ListAlertWebhooks)},
	{Method: POST, Path: "/v1/admin/alert-webhooks", Group: MerchantConfig, Auth: AuthMerchant, Name: "CreateAlertWebhook", Sensitive: true, When: FeatureMerchantConfigEdits,
		Request: billing.CreateAlertWebhookParams{}, Responses: []Reply{{201, billing.AlertWebhook{}}}, Errors: codes("service_unavailable", "webhook_invalid"), Handler: h(handlers.CreateAlertWebhook)},
	{Method: DELETE, Path: "/v1/admin/alert-webhooks/{id}", Group: MerchantConfig, Auth: AuthMerchant, Name: "DeleteAlertWebhook", Sensitive: true, When: FeatureMerchantConfigEdits,
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.DeleteAlertWebhook)},
	{Method: PATCH, Path: "/v1/admin/alert-webhooks/{id}", Group: MerchantConfig, Auth: AuthMerchant, Name: "UpdateAlertWebhook", Sensitive: true, When: FeatureMerchantConfigEdits,
		Request: billing.UpdateAlertWebhookParams{}, Responses: []Reply{{200, billing.AlertWebhook{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable", "webhook_invalid"), Handler: h(handlers.UpdateAlertWebhook)},

	// The portable billing archive. Archives own their snapshot/restore
	// transaction and its merchant pin; no outer merchant connection is held
	// while transferring the artifact.
	{Method: GET, Path: "/v1/admin/billing-archive", Group: MerchantConfig, Auth: AuthMerchant, Name: "ExportBillingArchive", Sensitive: true, NoConn: true,
		Responses: []Reply{{200, Stream{"application/x-ndjson"}}}, Errors: codes("billing_archive_unavailable", "billing_archive_unsupported_state"), Handler: h(handlers.ExportBillingArchive)},
	{Method: POST, Path: "/v1/admin/billing-archive", Group: MerchantConfig, Auth: AuthMerchant, Name: "ImportBillingArchive", Sensitive: true, NoConn: true,
		Request: Stream{"application/x-ndjson"}, Responses: []Reply{{200, billing.BillingArchiveImport{}}},
		Errors: codes("billing_archive_integrity", "billing_archive_invalid_artifact", "billing_archive_merchant_mismatch", "billing_archive_not_empty", "billing_archive_unavailable", "billing_archive_unsupported_state", "request_body_too_large"), Handler: h(handlers.ImportBillingArchive)},
	// #737: the DeclaredBilling import door. A bulk book import rewrites
	// subscriptions, payments and payment methods wholesale, so it is the
	// merchant's configuration; the import pins its own merchant connection.
	{Method: POST, Path: "/v1/admin/billing-import", Group: MerchantConfig, Auth: AuthMerchant, Name: "ImportBilling", Sensitive: true, NoConn: true,
		Request: billingimport.DeclaredBilling{}, Responses: []Reply{{200, billingimport.Result{}}}, Errors: codes("as_of_required", "invalid_param", "invalid_psp_reference", "resource_conflict"), Handler: h(handlers.ImportDeclaredBilling)},
}
