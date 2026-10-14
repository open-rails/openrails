package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// pspWriteErrors are the refusals of a PSP write, beside the route's own.
func pspWriteErrors(own ...string) []string {
	return codes(append(own, "invalid_param", "psp_credentials_rejected", "revision_mismatch", "service_unavailable")...)
}

// pspsRoutes is a merchant's PSPs (accounts on rails), the rails they can be
// armed on, and the callbacks providers send.
var pspsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/psps", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "ListPSPs",
		Query: params(queryOf(handlers.PSPListQuery{}), idsParam, integer("limit"), text("cursor")), Responses: []Reply{{200, billing.ListPage[billing.PSP]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_unavailable"), Handler: h(handlers.ListPSPs)},
	{Method: POST, Path: "/v1/merchant/psps", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "CreatePSP", When: FeatureMerchantConfigManages,
		Request: billing.CreatePSPParams{}, Responses: []Reply{{201, billing.PSP{}}}, Errors: pspWriteErrors("platform_vault_mixed_class", "platform_vault_unreachable", "psp_claim_requires_proof", "psp_environment_refused", "psp_exists", "psp_key_taken"), Handler: h(handlers.CreatePSP)},
	{Method: GET, Path: "/v1/merchant/psps/{id}", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "GetPSP",
		Responses: []Reply{{200, billing.PSP{}}}, Errors: codes("invalid_param", "psp_not_found", "service_unavailable"), Handler: h(handlers.GetPSP)},
	// Settings change, credentials rotate and the PSP archives here. Archiving
	// never calls the provider: a terminated account archives too.
	{Method: PATCH, Path: "/v1/merchant/psps/{id}", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "UpdatePSP", When: FeatureMerchantConfigManages,
		Request: billing.UpdatePSPParams{}, Responses: []Reply{{200, billing.PSP{}}}, Errors: pspWriteErrors("psp_last_active", "psp_not_found"), Handler: h(handlers.UpdatePSP)},
	// or#288: which PSP a purchase would get, and why. A projection of the
	// PSP catalog, so it takes the same read.
	{Method: POST, Path: "/v1/merchant/psps/routing-preview", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "PreviewPSPRouting",
		Request: billing.PreviewPSPRoutingParams{}, Responses: []Reply{{200, billing.PSPRoutingPreview{}}}, Errors: codes("invalid_param"), Handler: h(handlers.PreviewPSPRouting)},
	// A refresh rewrites the subscription mirrors from provider truth and
	// reads each PSP's catalog for drift, which it records as findings: a
	// heavy pull that may overwrite local state, so it is the merchant
	// configuration's.
	{Method: POST, Path: "/v1/merchant/psps/refresh", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "RefreshPSPs",
		Responses: []Reply{{202, billing.PSPRefresh{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.RefreshPSPs)},

	// The provider callback surface. The provider's account identity resolves
	// its merchant; runtime bindings and signatures remain mandatory. The body
	// is the provider's own payload.
	{Method: POST, Path: "/v1/webhooks/{rail}/{account_id}", Group: Webhooks, Auth: AuthProvider, NoConn: true,
		Query: params(text("eventType")), Request: Stream{"application/json"}, Responses: []Reply{{200, handlers.WebhookReceipt{}}}, Errors: codes("authentication_required", "invalid_param", "resource_access_denied", "service_unavailable", "webhook_account_mismatch"), Handler: h(handlers.Webhook)},
}
