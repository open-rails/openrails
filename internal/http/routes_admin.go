package server

import (
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/standalonehandlers"
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
	// The control plane's own merchant routes: API keys (#757), the team (#760)
	// and the merchant's name (#1106). Their handlers touch only the control
	// plane, never the runtime DB.
	if s.controlPlane != nil {
		control := opts
		control.External = httproutes.External{
			RenameMerchant:   router.Handler(standalonehandlers.MerchantRename(s.controlPlane)),
			CreateAPIKey:     router.Handler(standalonehandlers.MerchantCreateAPIKey(s.controlPlane)),
			ListAPIKeys:      router.Handler(standalonehandlers.MerchantListAPIKeys(s.controlPlane)),
			RevokeAPIKey:     router.Handler(standalonehandlers.MerchantRevokeAPIKey(s.controlPlane)),
			ListTeam:         router.Handler(standalonehandlers.MerchantListTeam(s.controlPlane)),
			ListTeamInvites:  router.Handler(standalonehandlers.MerchantListTeamInvites(s.controlPlane)),
			InviteTeamMember: router.Handler(standalonehandlers.MerchantInviteTeamMember(s.controlPlane)),
			RevokeTeamInvite: router.Handler(standalonehandlers.MerchantRevokeTeamInvite(s.controlPlane)),
			ChangeTeamRole:   router.Handler(standalonehandlers.MerchantChangeTeamRole(s.controlPlane)),
			RemoveTeamMember: router.Handler(standalonehandlers.MerchantRemoveTeamMember(s.controlPlane)),
		}
		httproutes.RegisterControlPlaneRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordRoute), s.runtime, control)
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
