package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/staffperm"
)

// merchantRoutes is the merchant itself: its configuration, API host,
// outbound webhooks, portable archive and bulk import, and, on the standalone
// server, its name, API keys, team and the signed-in user's merchants.
var merchantRoutes = []Route{
	{Method: GET, Path: "/v1/merchant/configuration", Group: MerchantConfig, Auth: AuthMerchant, Name: "GetMerchantConfiguration", Level: LevelAdmin, Resources: res(ResSettings), NoConn: true,
		Responses: []Reply{{200, billing.MerchantConfigurationState{}}}, Handler: h(handlers.GetMerchantConfiguration)},
	{Method: POST, Path: "/v1/merchant/configuration/applications", Group: MerchantConfig, Auth: AuthMerchant, Name: "ApplyMerchantConfiguration", Level: LevelAdmin, Resources: res(ResSettings), Sensitive: true, NoConn: true,
		Request: billing.ApplyMerchantConfigurationParams{}, Responses: []Reply{{200, billing.MerchantConfigurationReceipt{}}}, Errors: codes("invalid_param", "merchant_configuration_application_conflict", "merchant_configuration_revision_conflict"), Handler: h(handlers.ApplyMerchantConfiguration)},

	// A merchant's own API host (#734): claimed, proven by DNS, then served.
	{Method: GET, Path: "/v1/merchant/api-host", Group: MerchantConfig, Auth: AuthMerchant, Name: "GetAPIHost", Level: LevelAdmin, Resources: res(ResSettings), When: FeatureMerchantDirectory, NoConn: true,
		Responses: []Reply{{200, billing.MerchantAPIHost{}}}, Errors: codes("merchant_unresolved", "resource_not_found", "service_unavailable"), Handler: h(handlers.GetMerchantAPIHost)},
	{Method: PUT, Path: "/v1/merchant/api-host", Group: MerchantConfig, Auth: AuthMerchant, Name: "SetAPIHost", Level: LevelAdmin, Resources: res(ResSettings), Sensitive: true, When: FeatureMerchantDirectory, NoConn: true,
		Request: billing.SetAPIHostParams{}, Responses: []Reply{{200, billing.MerchantAPIHost{}}, {202, billing.MerchantAPIHost{}}}, Errors: codes("api_host_reserved", "api_host_taken", "invalid_api_host", "merchant_unresolved", "resource_not_found", "service_unavailable"), Handler: h(handlers.PutMerchantAPIHost)},
	{Method: POST, Path: "/v1/merchant/api-host/verify", Group: MerchantConfig, Auth: AuthMerchant, Name: "VerifyAPIHost", Level: LevelAdmin, Resources: res(ResSettings), Sensitive: true, When: FeatureMerchantDirectory, NoConn: true,
		Responses: []Reply{{200, billing.MerchantAPIHost{}}}, Errors: codes("api_host_claim_missing", "api_host_taken", "api_host_unproven", "merchant_unresolved", "resource_not_found", "service_unavailable"), Handler: h(handlers.VerifyMerchantAPIHost)},

	// Where the merchant's operational alerts are posted.
	{Method: GET, Path: "/v1/merchant/alert-webhooks", Group: MerchantConfig, Auth: AuthMerchant, Name: "ListAlertWebhooks", Level: LevelAdmin, Resources: res(ResSettings),
		Responses: []Reply{{200, billing.ListPage[billing.AlertWebhook]{}}}, Errors: codes("service_unavailable"), Handler: h(handlers.ListAlertWebhooks)},
	{Method: POST, Path: "/v1/merchant/alert-webhooks", Group: MerchantConfig, Auth: AuthMerchant, Name: "CreateAlertWebhook", Level: LevelAdmin, Resources: res(ResSettings), Sensitive: true,
		Request: billing.CreateAlertWebhookParams{}, Responses: []Reply{{201, billing.AlertWebhook{}}}, Errors: codes("service_unavailable", "webhook_invalid"), Handler: h(handlers.CreateAlertWebhook)},
	{Method: DELETE, Path: "/v1/merchant/alert-webhooks/{id}", Group: MerchantConfig, Auth: AuthMerchant, Name: "DeleteAlertWebhook", Level: LevelAdmin, Resources: res(ResSettings), Sensitive: true,
		Responses: []Reply{{204, nil}}, Errors: codes("invalid_param", "resource_not_found", "service_unavailable"), Handler: h(handlers.DeleteAlertWebhook)},
	{Method: PUT, Path: "/v1/merchant/alert-webhooks/{id}/url", Group: MerchantConfig, Auth: AuthMerchant, Name: "SetAlertWebhookURL", Level: LevelAdmin, Resources: res(ResSettings), Sensitive: true,
		Request: billing.SetAlertWebhookURLParams{}, Responses: []Reply{{200, billing.AlertWebhook{}}}, Errors: codes("invalid_param", "resource_conflict", "resource_not_found", "service_unavailable", "webhook_invalid"), Handler: h(handlers.SetAlertWebhookURL)},

	// The portable billing archive. Archives own their snapshot/restore
	// transaction and its merchant pin; no outer merchant connection is held
	// while transferring the artifact.
	{Method: GET, Path: "/v1/merchant/billing-archive", Group: MerchantConfig, Auth: AuthMerchant, Name: "ExportBillingArchive", Level: LevelAdmin, Resources: res(ResBillingData), Sensitive: true, NoConn: true,
		Responses: []Reply{{200, Stream{"application/x-ndjson"}}}, Errors: codes("billing_archive_unavailable", "billing_archive_unsupported_state"), Handler: h(handlers.ExportBillingArchive)},
	{Method: POST, Path: "/v1/merchant/billing-archive", Group: MerchantConfig, Auth: AuthMerchant, Name: "ImportBillingArchive", Level: LevelAdmin, Resources: res(ResBillingData), Sensitive: true, NoConn: true,
		Request: Stream{"application/x-ndjson"}, Responses: []Reply{{200, billing.BillingArchiveImport{}}},
		Errors: codes("billing_archive_integrity", "billing_archive_invalid_artifact", "billing_archive_merchant_mismatch", "billing_archive_not_empty", "billing_archive_unavailable", "billing_archive_unsupported_state", "request_body_too_large"), Handler: h(handlers.ImportBillingArchive)},
	// #737: the DeclaredBilling import door. A bulk book import rewrites
	// subscriptions, payments and payment methods wholesale, so it is the
	// merchant's configuration; the import pins its own merchant connection.
	{Method: POST, Path: "/v1/merchant/billing-import", Group: MerchantConfig, Auth: AuthMerchant, Name: "ImportBilling", Level: LevelAdmin, Resources: res(ResBillingData), Sensitive: true, NoConn: true,
		Request: billingimport.DeclaredBilling{}, Responses: []Reply{{200, billingimport.Result{}}}, Errors: codes("as_of_required", "invalid_param", "invalid_psp_reference", "resource_conflict"), Handler: h(handlers.ImportDeclaredBilling)},

	// The standalone control plane. Its handlers reach AuthKit, which the
	// catalog does not import: the standalone server supplies them.
	// #1106: a signed-in user's own merchants.
	{Method: GET, Path: "/v1/merchants", Group: ControlPlane, Auth: AuthUser, NoConn: true,
		Responses: []Reply{{200, billing.ListPage[billing.UserMerchant]{}}}, Errors: codes("authentication_required"), Bind: controlPlane(func(x *External) router.Handler { return x.ListMerchants })},
	{Method: POST, Path: "/v1/merchants", Group: ControlPlane, Auth: AuthUser, When: FeatureMerchantCreation, Throttle: ThrottleMerchantCreation, NoConn: true,
		Request: billing.CreateMerchantParams{}, Responses: []Reply{{200, billing.UserMerchant{}}, {201, billing.UserMerchant{}}},
		Errors: codes("authentication_required", "creation_refused", "email_unverified", "invalid_name", "merchant_creation_payment_method_required", "name_reserved", "name_taken"), Bind: controlPlane(func(x *External) router.Handler { return x.CreateMerchant })},
	// #1106: OpenRails owns merchant names; renaming is a settings change.
	{Method: PUT, Path: "/v1/merchant/name", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.Admin, Sensitive: true, NoConn: true,
		Request: billing.RenameMerchantParams{}, Responses: []Reply{{200, billing.MerchantName{}}}, Errors: codes("invalid_name", "name_reserved", "name_taken", "rename_too_soon", "renames_disabled", "resource_not_found"), Bind: controlPlane(func(x *External) router.Handler { return x.RenameMerchant })},
	// #757 merchant self-serve API keys: scoped credentials minted through
	// AuthKit, gated on the same permission its own mint authorization checks.
	{Method: POST, Path: "/v1/merchant/api-keys", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.CredentialsManage, Sensitive: true, NoConn: true,
		Request: billing.CreateAPIKeyParams{}, Responses: []Reply{{201, billing.CreatedAPIKey{}}}, Errors: codes("credential_revoked", "credentials_manage_required", "invalid_name", "merchant_unresolved", "role_escalation", "unknown_role"), Bind: controlPlane(func(x *External) router.Handler { return x.CreateAPIKey })},
	{Method: GET, Path: "/v1/merchant/api-keys", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.CredentialsManage, Sensitive: true, NoConn: true,
		Responses: []Reply{{200, billing.ListPage[billing.APIKey]{}}}, Errors: codes("merchant_unresolved"), Bind: controlPlane(func(x *External) router.Handler { return x.ListAPIKeys })},
	{Method: DELETE, Path: "/v1/merchant/api-keys/{id}", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.CredentialsManage, Sensitive: true, NoConn: true,
		Responses: []Reply{{204, nil}}, Errors: codes("credential_revoked", "merchant_unresolved", "resource_not_found", "role_escalation"), Bind: controlPlane(func(x *External) router.Handler { return x.RevokeAPIKey })},
	// #1140 federated grants: merchant roles granted by email to users of the
	// merchant's trusted issuers; the invitee accepts with a verified email.
	{Method: GET, Path: "/v1/merchant/federated-grants", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.MembersRead, NoConn: true,
		Responses: []Reply{{200, billing.ListPage[billing.FederatedGrant]{}}}, Errors: codes("merchant_unresolved"), Bind: controlPlane(func(x *External) router.Handler { return x.ListFederatedGrants })},
	{Method: POST, Path: "/v1/merchant/federated-grants", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.MembersManage, Sensitive: true, NoConn: true,
		Request: billing.CreateFederatedGrantParams{}, Responses: []Reply{{201, billing.FederatedGrant{}}}, Errors: codes("invalid_email", "merchant_unresolved", "resource_conflict", "role_escalation", "unknown_role"), Bind: controlPlane(func(x *External) router.Handler { return x.CreateFederatedGrant })},
	{Method: DELETE, Path: "/v1/merchant/federated-grants/{id}", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.MembersManage, Sensitive: true, NoConn: true,
		Responses: []Reply{{204, nil}}, Errors: codes("merchant_unresolved", "resource_not_found", "role_escalation"), Bind: controlPlane(func(x *External) router.Handler { return x.RevokeFederatedGrant })},
	{Method: GET, Path: "/v1/merchants/invites", Group: ControlPlane, Auth: AuthUser, NoConn: true,
		Responses: []Reply{{200, billing.ListPage[billing.FederatedInvite]{}}}, Errors: codes("authentication_required"), Bind: controlPlane(func(x *External) router.Handler { return x.ListMyFederatedGrants })},
	{Method: POST, Path: "/v1/merchants/invites/{id}/accept", Group: ControlPlane, Auth: AuthUser, NoConn: true,
		Responses: []Reply{{200, billing.UserMerchant{}}}, Errors: codes("authentication_required", "email_unverified", "resource_conflict", "resource_not_found"), Bind: controlPlane(func(x *External) router.Handler { return x.AcceptFederatedGrant })},
	// #760 merchant team: roster, invitations, role changes and removal,
	// through AuthKit group membership.
	{Method: GET, Path: "/v1/merchant/team", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.MembersRead, NoConn: true,
		Responses: []Reply{{200, billing.ListPage[billing.TeamMember]{}}}, Errors: codes("merchant_unresolved"), Bind: controlPlane(func(x *External) router.Handler { return x.ListTeam })},
	{Method: GET, Path: "/v1/merchant/team/invites", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.MembersRead, NoConn: true,
		Responses: []Reply{{200, billing.ListPage[billing.TeamInvite]{}}}, Errors: codes("merchant_unresolved"), Bind: controlPlane(func(x *External) router.Handler { return x.ListTeamInvites })},
	{Method: POST, Path: "/v1/merchant/team/invites", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.MembersManage, Sensitive: true, NoConn: true,
		Request: billing.InviteTeamMemberParams{}, Responses: []Reply{{201, billing.TeamInviteResult{}}}, Errors: codes("credential_revoked", "invalid_email", "invites_disabled", "members_manage_required", "merchant_unresolved", "role_escalation", "unknown_role"), Bind: controlPlane(func(x *External) router.Handler { return x.InviteTeamMember })},
	{Method: DELETE, Path: "/v1/merchant/team/invites/{id}", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.MembersManage, Sensitive: true, NoConn: true,
		Responses: []Reply{{204, nil}}, Errors: codes("credential_revoked", "members_manage_required", "merchant_unresolved", "resource_not_found"), Bind: controlPlane(func(x *External) router.Handler { return x.RevokeTeamInvite })},
	{Method: PATCH, Path: "/v1/merchant/team/{user_id}", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.MembersManage, Sensitive: true, NoConn: true,
		Request: billing.SetTeamRoleParams{}, Responses: []Reply{{200, billing.TeamMember{}}}, Errors: codes("credential_revoked", "invalid_user", "last_owner", "members_manage_required", "merchant_unresolved", "resource_not_found", "role_escalation", "unknown_role"), Bind: controlPlane(func(x *External) router.Handler { return x.ChangeTeamRole })},
	{Method: DELETE, Path: "/v1/merchant/team/{user_id}", Group: ControlPlane, Auth: AuthMerchant, Perm: staffperm.MembersManage, Sensitive: true, NoConn: true,
		Responses: []Reply{{204, nil}}, Errors: codes("credential_revoked", "invalid_user", "last_owner", "members_manage_required", "merchant_unresolved", "resource_not_found"), Bind: controlPlane(func(x *External) router.Handler { return x.RemoveTeamMember })},
}
