package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// merchantRoutes is the merchant itself: its configuration, outbound
// webhooks, portable archive and bulk import.
var merchantRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/configuration", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "GetMerchantConfiguration", NoConn: true,
		Cache: config.ReadMerchantConfiguration,
		Doc: "GetMerchantConfiguration reads the merchant's configuration, never its credentials, and its " +
			"revision.",
		Responses: []Reply{{200, billing.MerchantConfigurationState{}}}, Handler: h(handlers.GetMerchantConfiguration)},
	// Merchant configuration changes only where Vault holds it: a file is
	// read-only.
	{Method: PATCH, Path: "/v1/merchant/configuration", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "UpdateMerchantConfiguration", When: FeatureMerchantConfigManages, NoConn: true,
		Doc: "UpdateMerchantConfiguration merges params into the merchant's configuration and returns it. It needs " +
			"merchant configuration held in Vault: a configuration read from a file is read-only.",
		Request: billing.UpdateMerchantConfigurationParams{}, Responses: []Reply{{200, billing.MerchantConfigurationState{}}}, Errors: codes("card_vault_unknown", "charge_psp_incompatible", "charge_psp_unknown", "event_store_required", "invalid_param", "merchant_unit_unsupported", "platform_vault_mixed_class", "platform_vault_unavailable", "platform_vault_unreachable", "resource_conflict", "revision_mismatch", "service_unavailable"), Handler: h(handlers.UpdateMerchantConfiguration)},

	// What changed in the catalog and the configuration, and who changed it.
	{Method: GET, Path: "/v1/merchant/changes", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "ListChanges",
		Doc: "ListChanges lists the merchant's configuration history, newest first: each change to a product, price, " +
			"meter or limit and to the merchant, messages, PSP and custodian documents, with who made it and how. A credential " +
			"reads \"changed\", never its value. ObjectType and ObjectID narrow it to one object, Since and Until to a " +
			"time range.",
		Query: billing.LogListParams{}, Responses: []Reply{{200, billing.ListPage[billing.LogEntry]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_unavailable"), Handler: h(handlers.ListChanges)},

	// Where the merchant's operational alerts are posted.
	{Method: GET, Path: "/v1/merchant/alert-webhooks", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "ListAlertWebhooks",
		Doc:   "ListAlertWebhooks returns where the merchant's operational alerts are posted.",
		Query: billing.AlertWebhookListParams{}, Responses: []Reply{{200, billing.ListPage[billing.AlertWebhook]{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.ListAlertWebhooks)},
	{Method: POST, Path: "/v1/merchant/alert-webhooks", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "CreateAlertWebhook", When: FeatureMerchantConfigManages,
		Doc:     "CreateAlertWebhook adds a destination for the merchant's operational alerts.",
		Request: billing.CreateAlertWebhookParams{}, Responses: []Reply{{201, billing.AlertWebhook{}}}, Errors: codes("service_unavailable", "webhook_invalid"), Handler: h(handlers.CreateAlertWebhook)},
	{Method: DELETE, Path: "/v1/merchant/alert-webhooks/{id}", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "DeleteAlertWebhook", When: FeatureMerchantConfigManages,
		Args:      []any{billing.AlertWebhookID{}},
		Doc:       "DeleteAlertWebhook removes an alert webhook.",
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.DeleteAlertWebhook)},
	{Method: PATCH, Path: "/v1/merchant/alert-webhooks/{id}", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "UpdateAlertWebhook", When: FeatureMerchantConfigManages,
		Args:    []any{billing.AlertWebhookID{}},
		Doc:     "UpdateAlertWebhook changes an alert webhook; omitted fields keep their values.",
		Request: billing.UpdateAlertWebhookParams{}, Responses: []Reply{{200, billing.AlertWebhook{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable", "webhook_invalid"), Handler: h(handlers.UpdateAlertWebhook)},

	// The portable billing archive. Archives own their snapshot/restore
	// transaction and its merchant pin; no outer merchant connection is held
	// while transferring the artifact.
	{Method: GET, Path: "/v1/merchant/billing-archive", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "ExportBillingArchive", NoConn: true,
		Client: HandWritten,
		Doc: "ExportBillingArchive writes the bound merchant's versioned billing archive. Stop source writers for " +
			"the final cutover export. Authentication identities, provider credentials and live jobs are not " +
			"portable billing records.\n\n" +
			"The writer may contain incomplete bytes when an error is returned; publish a file only after " +
			"success. The Client verifies the archive's integrity footer, and uses the same credential, merchant " +
			"binding and timeout as other methods.",
		Responses: []Reply{{200, Stream{"application/x-ndjson"}}}, Errors: codes("billing_archive_platform_cards", "billing_archive_unavailable", "billing_archive_unsupported_state"), Handler: h(handlers.ExportBillingArchive)},
	{Method: POST, Path: "/v1/merchant/billing-archive", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "ImportBillingArchive", NoConn: true,
		Client: HandWritten,
		Doc: "ImportBillingArchive atomically restores an archive into an empty destination with the same merchant " +
			"UUID. Provision destination authority separately and keep destination workers stopped through " +
			"restoration and reconfiguration. No provider call is made. A lost response can be retried with the " +
			"same archive.",
		Request: Stream{"application/x-ndjson"}, Responses: []Reply{{200, billing.BillingArchiveImport{}}},
		Errors: codes("billing_archive_integrity", "billing_archive_invalid_artifact", "billing_archive_merchant_mismatch", "billing_archive_not_empty", "billing_archive_platform_cards", "billing_archive_unavailable", "billing_archive_unsupported_state", "request_body_too_large"), Handler: h(handlers.ImportBillingArchive)},
	// #737: the DeclaredBilling import door. A bulk book import rewrites
	// subscriptions, payments and payment methods wholesale, so it is the
	// merchant's configuration; the import pins its own merchant connection.
	{Method: POST, Path: "/v1/merchant/billing-import", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "ImportBilling", NoConn: true,
		Doc:     "ImportBilling lands declared billing facts under the bound merchant.",
		Request: billingimport.DeclaredBilling{}, Responses: []Reply{{200, billingimport.Result{}}}, Errors: codes("as_of_required", "invalid_param", "invalid_psp_reference", "resource_conflict"), Handler: h(handlers.ImportDeclaredBilling)},

	// PSP imports: pull a PSP's subscribers read-only, link them to customers
	// by the merchant's rules and mappings, and land the linked ones through
	// the billing import.
	{Method: POST, Path: "/v1/merchant/psp-imports", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "CreatePSPImport",
		Doc: "CreatePSPImport pulls a PSP: OpenRails observes its catalog, pulls its customers, saved cards, live " +
			"subscriptions and transaction history (reads only), links what it does not hold to customers by the " +
			"import's rules, and proposes every difference in what it holds (ListProposals with Pull). One import " +
			"per PSP pulls or applies at a time. The pull runs in the background; GetPSPImport reports it. " +
			"ListProviderCatalog with Import then lists the plans to map.",
		Request: billing.CreatePSPImportParams{}, Responses: []Reply{{202, billing.PSPImport{}}}, Errors: codes("invalid_param", "psp_import_in_progress", "psp_not_found", "service_unavailable"), Handler: h(handlers.CreatePSPImport)},
	{Method: GET, Path: "/v1/merchant/psp-imports", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "ListPSPImports",
		Doc:   "ListPSPImports lists the merchant's PSP imports, newest first: one PSP's when set.",
		Query: billing.PSPImportListParams{}, Responses: []Reply{{200, billing.ListPage[billing.PSPImport]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "psp_not_found", "service_unavailable"), Handler: h(handlers.ListPSPImports)},
	{Method: GET, Path: "/v1/merchant/psp-imports/{id}", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "GetPSPImport",
		Args:      []any{billing.PSPImportID{}},
		Doc:       "GetPSPImport reads one PSP import with its records counted.",
		Responses: []Reply{{200, billing.PSPImport{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetPSPImport)},
	{Method: PATCH, Path: "/v1/merchant/psp-imports/{id}", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "UpdatePSPImport",
		Args:    []any{billing.PSPImportID{}},
		Doc:     "UpdatePSPImport replaces the import's rules; LinkPSPImport applies them.",
		Request: billing.UpdatePSPImportParams{}, Responses: []Reply{{200, billing.PSPImport{}}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.UpdatePSPImport)},
	{Method: DELETE, Path: "/v1/merchant/psp-imports/{id}", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "DeletePSPImport",
		Args:      []any{billing.PSPImportID{}},
		Doc:       "DeletePSPImport deletes an import and its staged records; what it landed stays.",
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.DeletePSPImport)},
	{Method: GET, Path: "/v1/merchant/psp-imports/{id}/records", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "ListPSPImportRecords",
		Args: []any{billing.PSPImportID{}},
		Doc: "ListPSPImportRecords lists an import's staged provider objects, by state or kind: the unmatched ones " +
			"are what no rule linked.",
		Query: billing.PSPImportRecordListParams{}, Responses: []Reply{{200, billing.ListPage[billing.PSPImportRecord]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "resource_not_found", "service_unavailable"), Handler: h(handlers.ListPSPImportRecords)},
	{Method: POST, Path: "/v1/merchant/psp-imports/{id}/link", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "LinkPSPImport",
		Args: []any{billing.PSPImportID{}},
		Doc: "LinkPSPImport stores manual links and price picks, then relinks the import in the background: every " +
			"staged record not yet imported is linked again by the import's rules and the merchant's mappings, " +
			"and the ones OpenRails holds are marked held. The import is pulling until it has; GetPSPImport " +
			"reports it.",
		Request: billing.LinkPSPImportParams{}, Responses: []Reply{{202, billing.PSPImport{}}}, Errors: codes("invalid_param", "psp_import_in_progress", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.LinkPSPImport)},
	{Method: POST, Path: "/v1/merchant/psp-imports/{id}/apply", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "ApplyPSPImport", Person: true,
		Args: []any{billing.PSPImportID{}},
		Doc: "ApplyPSPImport lands the import's linked records as customers, payment methods, subscriptions and " +
			"payments, then approves its corrections (the proposals that keep or extend access) in the caller's " +
			"name. Only a person signed in, or the in-process host, applies. It runs in the background. A " +
			"subscription lands on the provider price mapping its plan; one whose plan is unmapped stays blocked " +
			"until it is mapped (CreateProviderPrice), linked and applied again.",
		Responses: []Reply{{202, billing.PSPImport{}}}, Errors: codes("invalid_param", "person_required", "psp_import_in_progress", "resource_conflict", "resource_not_found", "service_unavailable"), Handler: h(handlers.ApplyPSPImport)},
	{Method: POST, Path: "/v1/merchant/psp-import-mappings", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "SetPSPImportMappings",
		Doc: "SetPSPImportMappings maps the merchant's own identifiers (a legacy user or order id) to customers, " +
			"for import rules to read.",
		Request: billing.SetPSPImportMappingsParams{}, Responses: []Reply{{200, billing.PSPImportMappingsResult{}}}, Errors: codes("invalid_param", "service_unavailable"), Handler: h(handlers.SetPSPImportMappings)},
	// What each PSP's own catalog says, as last observed: the import's plans
	// to map, and which provider price maps each.
	{Method: GET, Path: "/v1/merchant/provider-catalog", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "ListProviderCatalog",
		Doc: "ListProviderCatalog lists one PSP's own catalog (Stripe products and prices, NMI plans) as OpenRails " +
			"last observed it, each price with the provider price mapping it. With Import it lists the plans that " +
			"import's subscriptions name: map each by creating a provider price.",
		Query: billing.ProviderCatalogListParams{}, Responses: []Reply{{200, billing.ListPage[billing.ProviderCatalogObject]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "psp_not_found", "resource_not_found", "service_unavailable"), Handler: h(handlers.ListProviderCatalog)},
	// Live provider subscriptions converge could not land (unmapped price,
	// unlinked customer, product already held), open or closed.
	{Method: GET, Path: "/v1/merchant/parked-subscriptions", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "ListParkedSubscriptions",
		Doc: "ListParkedSubscriptions lists the live provider subscriptions OpenRails could not land, newest " +
			"first: open parks, or closed ones with Released. Mapping a park's price or linking its customer " +
			"lands it.",
		Query: billing.ParkedSubscriptionListParams{}, Responses: []Reply{{200, billing.ListPage[billing.ParkedSubscription]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "psp_not_found", "service_unavailable"), Handler: h(handlers.ListParkedSubscriptions)},
}
