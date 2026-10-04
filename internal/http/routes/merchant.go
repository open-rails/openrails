package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/modules/alerting"
)

// merchantRoutes is the merchant itself: its configuration, API host,
// outbound webhooks, portable archive and bulk import, and, on the standalone
// server, its name, API keys, team and the signed-in user's merchants.
var merchantRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/configuration", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsRead, NoConn: true,
		Responses: []Reply{{200, billing.MerchantConfigurationState{}}}, Handler: h(handlers.GetMerchantConfiguration)},
	{Method: POST, Path: "/v1/merchant/configuration/applications", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsUpdate, NoConn: true,
		Request: billing.MerchantConfigurationApplyParams{}, Responses: []Reply{{200, billing.MerchantConfigurationReceipt{}}}, Errors: codes("api_host_requires_proof", "invalid_param", "merchant_configuration_application_conflict", "merchant_configuration_revision_conflict"), Handler: h(handlers.ApplyMerchantConfiguration)},
	{Method: GET, Path: "/v1/merchant/settings", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsRead,
		Responses: []Reply{{200, billing.MerchantSettings{}}}, Handler: h(handlers.ServiceGetMerchantSettings)},
	{Method: PUT, Path: "/v1/merchant/settings", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsUpdate,
		Request: billing.MerchantSettings{}, Responses: []Reply{{200, Message{}}}, Errors: codes("invalid_param"), Handler: h(handlers.ServiceSetMerchantSettings)},

	// A merchant's own API host (#734): claimed, proven by DNS, then served.
	{Method: GET, Path: "/v1/merchant/api-host", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsRead, When: FeatureMerchantDirectory, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("merchant_unresolved", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetMerchantAPIHost)},
	{Method: PUT, Path: "/v1/merchant/api-host", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsUpdate, When: FeatureMerchantDirectory, NoConn: true,
		Request: Untyped{}, Responses: []Reply{{200, Untyped{}}, {202, Untyped{}}}, Errors: codes("api_host_reserved", "api_host_taken", "invalid_api_host", "merchant_unresolved", "resource_not_found", "service_unavailable"), Handler: h(handlers.PutMerchantAPIHost)},
	{Method: POST, Path: "/v1/merchant/api-host/verify", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsUpdate, When: FeatureMerchantDirectory, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("api_host_claim_missing", "api_host_taken", "api_host_unproven", "merchant_unresolved", "resource_not_found", "service_unavailable"), Handler: h(handlers.VerifyMerchantAPIHost)},

	// Outbound notification destinations.
	{Method: GET, Path: "/v1/merchant/webhooks", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsRead,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.ListMerchantWebhooks)},
	{Method: POST, Path: "/v1/merchant/webhooks", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsUpdate,
		Request: alerting.CreateWebhookInput{}, Responses: []Reply{{201, alerting.Webhook{}}}, Errors: codes("resource_not_found", "service_unavailable", "webhook_invalid"), Handler: h(handlers.CreateMerchantWebhook)},
	{Method: DELETE, Path: "/v1/merchant/webhooks/{id}", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsUpdate,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("resource_not_found", "service_unavailable"), Handler: h(handlers.DeleteMerchantWebhook)},
	{Method: PUT, Path: "/v1/merchant/webhooks/{id}/url", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantSettingsUpdate,
		Request: alerting.RotateWebhookURLInput{}, Responses: []Reply{{200, alerting.Webhook{}}}, Errors: codes("resource_conflict", "resource_not_found", "service_unavailable", "webhook_invalid"), Handler: h(handlers.RotateMerchantWebhookURL)},

	// The portable billing archive. Archives own their snapshot/restore
	// transaction and its merchant pin; no outer merchant connection is held
	// while transferring the artifact.
	{Method: GET, Path: "/v1/merchant/billing-archive", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantBillingExport, NoConn: true,
		Responses: []Reply{{200, Stream{"application/x-ndjson"}}}, Errors: codes("billing_archive_unavailable", "billing_archive_unsupported_state"), Handler: h(handlers.ExportMerchantBilling)},
	{Method: POST, Path: "/v1/merchant/billing-archive", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantBillingImport, NoConn: true,
		Request: Stream{"application/x-ndjson"}, Responses: []Reply{{200, billing.MerchantBillingImportResult{}}},
		Errors: codes("billing_archive_integrity", "billing_archive_invalid_artifact", "billing_archive_merchant_mismatch", "billing_archive_not_empty", "billing_archive_unavailable", "billing_archive_unsupported_state", "request_body_too_large"), Handler: h(handlers.ImportMerchantBilling)},
	// #737: the DeclaredBilling import door. A bulk book import rewrites
	// subscriptions, payments and payment methods wholesale, so it has its own
	// owner-level grant; the import pins its own merchant connection.
	{Method: POST, Path: "/v1/import/billing", Group: Merchant, Auth: AuthMerchant, Perm: billing.MerchantBillingImport, NoConn: true,
		Request: billingimport.DeclaredBilling{}, Responses: []Reply{{200, billingimport.Result{}}}, Errors: codes("as_of_required", "invalid_param", "invalid_psp_reference", "resource_conflict"), Handler: h(handlers.ImportDeclaredBilling)},

	// The standalone control plane. Its handlers reach AuthKit, which the
	// catalog does not import: the standalone server supplies them.
	// #1106: a signed-in user's own merchants.
	{Method: GET, Path: "/v1/merchants", Group: ControlPlane, Auth: AuthUser, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("authentication_required"), Bind: controlPlane(func(x *External) router.Handler { return x.ListMerchants })},
	{Method: POST, Path: "/v1/merchants", Group: ControlPlane, Auth: AuthUser, When: FeatureMerchantCreation, Throttle: ThrottleMerchantCreation, NoConn: true,
		Request: Untyped{}, Responses: []Reply{{200, Untyped{}}, {201, Untyped{}}},
		Errors: codes("authentication_required", "creation_refused", "email_unverified", "invalid_name", "merchant_creation_payment_method_required", "name_reserved", "name_taken"), Bind: controlPlane(func(x *External) router.Handler { return x.CreateMerchant })},
	// #1106: OpenRails owns merchant names; renaming is a settings change.
	{Method: PUT, Path: "/v1/merchant/name", Group: ControlPlane, Auth: AuthMerchant, Perm: billing.MerchantSettingsUpdate, NoConn: true,
		Request: Untyped{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("invalid_name", "name_reserved", "name_taken", "rename_too_soon", "renames_disabled", "resource_not_found"), Bind: controlPlane(func(x *External) router.Handler { return x.RenameMerchant })},
	// #757 merchant self-serve API keys: scoped credentials minted through
	// AuthKit, gated on the same permission its own mint authorization checks.
	{Method: POST, Path: "/v1/merchant/api-keys", Group: ControlPlane, Auth: AuthMerchant, Perm: billing.MerchantCredentialsManage, NoConn: true,
		Request: Untyped{}, Responses: []Reply{{201, Untyped{}}}, Errors: codes("credential_revoked", "credentials_manage_required", "invalid_name", "merchant_unresolved", "role_escalation", "unknown_role"), Bind: controlPlane(func(x *External) router.Handler { return x.CreateAPIKey })},
	{Method: GET, Path: "/v1/merchant/api-keys", Group: ControlPlane, Auth: AuthMerchant, Perm: billing.MerchantCredentialsManage, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("merchant_unresolved"), Bind: controlPlane(func(x *External) router.Handler { return x.ListAPIKeys })},
	{Method: DELETE, Path: "/v1/merchant/api-keys/{id}", Group: ControlPlane, Auth: AuthMerchant, Perm: billing.MerchantCredentialsManage, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("credential_revoked", "merchant_unresolved", "resource_not_found", "role_escalation"), Bind: controlPlane(func(x *External) router.Handler { return x.RevokeAPIKey })},
	// #760 merchant team: roster, invitations, role changes and removal,
	// through AuthKit group membership.
	{Method: GET, Path: "/v1/merchant/team", Group: ControlPlane, Auth: AuthMerchant, Perm: billing.MerchantMembersRead, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("merchant_unresolved"), Bind: controlPlane(func(x *External) router.Handler { return x.ListTeam })},
	{Method: GET, Path: "/v1/merchant/team/invites", Group: ControlPlane, Auth: AuthMerchant, Perm: billing.MerchantMembersRead, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("merchant_unresolved"), Bind: controlPlane(func(x *External) router.Handler { return x.ListTeamInvites })},
	{Method: POST, Path: "/v1/merchant/team/invites", Group: ControlPlane, Auth: AuthMerchant, Perm: billing.MerchantMembersManage, NoConn: true,
		Request: Untyped{}, Responses: []Reply{{201, Untyped{}}}, Errors: codes("credential_revoked", "invalid_email", "invites_disabled", "members_manage_required", "merchant_unresolved", "role_escalation", "unknown_role"), Bind: controlPlane(func(x *External) router.Handler { return x.InviteTeamMember })},
	{Method: DELETE, Path: "/v1/merchant/team/invites/{id}", Group: ControlPlane, Auth: AuthMerchant, Perm: billing.MerchantMembersManage, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("credential_revoked", "members_manage_required", "merchant_unresolved", "resource_not_found"), Bind: controlPlane(func(x *External) router.Handler { return x.RevokeTeamInvite })},
	{Method: PATCH, Path: "/v1/merchant/team/{user_id}", Group: ControlPlane, Auth: AuthMerchant, Perm: billing.MerchantMembersManage, NoConn: true,
		Request: Untyped{}, Responses: []Reply{{200, Untyped{}}}, Errors: codes("credential_revoked", "invalid_user", "last_owner", "members_manage_required", "merchant_unresolved", "resource_not_found", "role_escalation", "unknown_role"), Bind: controlPlane(func(x *External) router.Handler { return x.ChangeTeamRole })},
	{Method: DELETE, Path: "/v1/merchant/team/{user_id}", Group: ControlPlane, Auth: AuthMerchant, Perm: billing.MerchantMembersManage, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Errors: codes("credential_revoked", "invalid_user", "last_owner", "members_manage_required", "merchant_unresolved", "resource_not_found"), Bind: controlPlane(func(x *External) router.Handler { return x.RemoveTeamMember })},
}
