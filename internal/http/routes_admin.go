package server

import (
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/standalonehandlers"
	"github.com/open-rails/openrails/permissions"
	"net/http"
)

func (s *Server) registerMerchantActionRoutesAt(mux router.Registrar, apiPrefix string) {
	prefix := apiPrefix + "/merchant"
	opts := httproutes.Options{
		Gate: httproutes.NewGate(httproutes.GateOptions{
			Authenticator:             s.authenticator,
			AdminPermissionChecker:    s.controlPlane,
			ServiceCredentialResolver: s.controlPlane,
			DelegatedResolver:         s.controlPlane,
			DelegatedAuthenticator:    s.delegatedAuthenticator,
		}),
		AdminLimiter: s.adminLimiter,
	}
	rr := router.NewMuxRecorded(mux, prefix, s.runtime, s.recordRoute)
	// #757 merchant self-serve API keys: mint/list/revoke scoped credentials
	// through AuthKit core. Gated on merchant:credentials:manage — the SAME
	// string AuthKit's own mint authorization checks; only the merchant owner
	// (merchant:*) holds it in the fixed #567 catalog. No MerchantDBConnMW:
	// these handlers touch only the control plane, never the runtime DB.
	if s.controlPlane != nil {
		credentialsManage := opts.RequireMerchantPermission(permissions.MerchantCredentialsManage)
		apiKeys := rr.Group("/api-keys")
		apiKeys.Handle(http.MethodPost, "", router.Handler(standalonehandlers.MerchantCreateAPIKey(s.controlPlane)), credentialsManage)
		apiKeys.Handle(http.MethodGet, "", router.Handler(standalonehandlers.MerchantListAPIKeys(s.controlPlane)), credentialsManage)
		apiKeys.Handle(http.MethodDelete, "/:id", router.Handler(standalonehandlers.MerchantRevokeAPIKey(s.controlPlane)), credentialsManage)
	}

	// #760 merchant team management: roster, invites (register+join links),
	// role changes, and removal — all through AuthKit group membership. Reads
	// gate on merchant:members:read; mutations on merchant:members:manage. Only
	// the owner (merchant:*) holds either in the fixed #567 catalog, so the whole
	// surface is owner-only (mirrors #757). No MerchantDBConnMW: control plane
	// only. `/team/invites` (literal) and `/team/:user_id` never collide — they
	// differ by segment shape/method.
	if s.controlPlane != nil {
		membersRead := opts.RequireMerchantPermission(permissions.MerchantMembersRead)
		membersManage := opts.RequireMerchantPermission(permissions.MerchantMembersManage)
		team := rr.Group("/team")
		team.Handle(http.MethodGet, "", router.Handler(standalonehandlers.MerchantListTeam(s.controlPlane)), membersRead)
		team.Handle(http.MethodGet, "/invites", router.Handler(standalonehandlers.MerchantListTeamInvites(s.controlPlane)), membersRead)
		team.Handle(http.MethodPost, "/invites", router.Handler(standalonehandlers.MerchantInviteTeamMember(s.controlPlane)), membersManage)
		team.Handle(http.MethodDelete, "/invites/:id", router.Handler(standalonehandlers.MerchantRevokeTeamInvite(s.controlPlane)), membersManage)
		team.Handle(http.MethodPatch, "/:user_id", router.Handler(standalonehandlers.MerchantChangeTeamRole(s.controlPlane)), membersManage)
		team.Handle(http.MethodDelete, "/:user_id", router.Handler(standalonehandlers.MerchantRemoveTeamMember(s.controlPlane)), membersManage)
	}

	// #555 HARD CUT: the merchant API surface is `/v1/merchant/*`. Standalone
	// mounts every merchant route set here: human admin/support, settings/catalog,
	// and the machine billing API.
	httproutes.RegisterMerchantActionRoutes(router.NewMuxRecorded(mux, prefix, s.runtime, s.recordRoute), s.runtime, opts)
	httproutes.RegisterCatalogRoutes(router.NewMuxRecorded(mux, prefix+"/catalog", s.runtime, s.recordRoute), s.runtime, opts)
	httproutes.RegisterCatalogCollectionRoutes(router.NewMuxRecorded(mux, prefix+"/catalogs", s.runtime, s.recordRoute), s.runtime, opts)
	httproutes.RegisterOwnedCatalogRoutes(router.NewMuxRecorded(mux, apiPrefix+"/catalog", s.runtime, s.recordRoute), s.runtime, opts)
	if s.runtime.Config.MerchantConfigHTTP {
		httproutes.RegisterMerchantConfigRoutes(router.NewMuxRecorded(mux, prefix, s.runtime, s.recordRoute), s.runtime, opts)
	}
	httproutes.RegisterServiceRoutes(router.NewMuxRecorded(mux, prefix, s.runtime, s.recordRoute), s.runtime, opts)
	// #737: DeclaredBilling import (POST <api>/import/billing), merchant from
	// the authenticated credential like every other merchant-scoped route.
	httproutes.RegisterImportRoutes(router.NewMuxRecorded(mux, apiPrefix+"/import", s.runtime, s.recordRoute), s.runtime, opts)
}

func (s *Server) registerMerchantActionRoutes(mux router.Registrar) {
	s.registerMerchantActionRoutesAt(mux, StandaloneV1Prefix)
}
