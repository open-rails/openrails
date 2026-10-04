package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// pspWriteErrors are the refusals of a PSP write, beside the route's own.
func pspWriteErrors(own ...string) []string {
	return codes(append(own, "credential_custody_transition_required", "credential_operation_conflict", "credential_source_read_only", "credential_store_read_only", "invalid_param", "psp_credentials_rejected", "service_unavailable")...)
}

// pspsRoutes is a merchant's PSPs (accounts on rails), the rails they can be
// armed on, and the callbacks providers send.
var pspsRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/psps", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPSPsRead,
		Query: params(queryOf(handlers.PSPListQuery{}), integer("limit"), text("cursor")), Responses: []Reply{{200, billing.ListPage[billing.PSP]{}}}, Errors: codes("invalid_cursor", "invalid_param", "invalid_query", "service_unavailable"), Handler: h(handlers.ListPSPs)},
	{Method: POST, Path: "/v1/merchant/psps", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPSPsUpdate,
		Request: billing.CreatePSPParams{}, Responses: []Reply{{201, billing.PSP{}}}, Errors: pspWriteErrors("psp_claim_requires_proof", "psp_exists", "psp_key_taken"), Handler: h(handlers.CreatePSP)},
	{Method: GET, Path: "/v1/merchant/psps/{psp_id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPSPsRead,
		Responses: []Reply{{200, billing.PSP{}}}, Errors: codes("invalid_param", "psp_not_found", "service_unavailable"), Handler: h(handlers.GetPSP)},
	// Credentials rotate here; settings changes work with a read-only
	// credential backend.
	{Method: PATCH, Path: "/v1/merchant/psps/{psp_id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPSPsUpdate,
		Request: billing.UpdatePSPParams{}, Responses: []Reply{{200, billing.PSP{}}}, Errors: pspWriteErrors("psp_not_found"), Handler: h(handlers.UpdatePSP)},
	// Archiving writes only the PSP row, never a secret, never the provider:
	// a terminated account archives from any deployment.
	{Method: POST, Path: "/v1/merchant/psps/{psp_id}/archive", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPSPsUpdate,
		Request: billing.ArchivePSPParams{}, Responses: []Reply{{200, billing.PSP{}}}, Errors: codes("invalid_param", "psp_last_active", "psp_not_found", "service_unavailable"), Handler: h(handlers.ArchivePSP)},
	// or#288: which PSP a checkout would get, and why. A projection of the
	// PSP catalog, so it takes the same read.
	{Method: POST, Path: "/v1/merchant/psps/routing-preview", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPSPsRead,
		Request: billing.PSPRoutingPreviewParams{}, Responses: []Reply{{200, billing.PSPRoutingPreview{}}}, Errors: codes("catalog_scope_mismatch", "invalid_param"), Handler: h(handlers.PreviewPSPRouting)},
	// A refresh rewrites the subscription mirrors from provider truth.
	{Method: POST, Path: "/v1/merchant/psps/refresh", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSubscriptionsUpdate,
		Responses: []Reply{{202, billing.PSPRefresh{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.RefreshPSPs)},
	{Method: GET, Path: "/v1/merchant/rails", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantPSPsRead,
		Responses: []Reply{{200, billing.ListPage[billing.RailDefinition]{}}}, Handler: h(handlers.ListRails)},

	// The provider callback surface. The provider's account identity resolves
	// its merchant; runtime bindings and signatures remain mandatory. The body
	// is the provider's own payload.
	{Method: POST, Path: "/v1/webhooks/{rail}/{account_id}", Group: Webhooks, Auth: AuthProvider, NoConn: true,
		Query: params(text("eventType")), Request: Untyped{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required", "credential_custody_transition_required", "invalid_param", "resource_access_denied", "service_unavailable", "webhook_account_mismatch"), Handler: h(handlers.Webhook)},
}
