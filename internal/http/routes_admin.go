package server

import (
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/staffperm"
	"github.com/open-rails/openrails/internal/standalonehandlers"
)

// staffPermissions are the server's own permissions on its merchant persona.
var staffPermissions = httproutes.Permissions{AdminRead: staffperm.Read, AdminWrite: staffperm.Write, CatalogWrite: staffperm.Admin, MerchantConfig: staffperm.Admin}

func (s *Server) registerMerchantActionRoutesAt(mux router.Registrar, apiPrefix string) {
	opts := httproutes.Options{
		Auth:              s.staffAuth(),
		AuthBindsMerchant: true,
		AdminLimiter:      s.adminLimiter,
		Capabilities:      s.capabilities(),
		Permissions:       staffPermissions,
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

			ListFederatedGrants:  router.Handler(standalonehandlers.MerchantListFederatedGrants(s.controlPlane)),
			CreateFederatedGrant: router.Handler(standalonehandlers.MerchantCreateFederatedGrant(s.controlPlane)),
			RevokeFederatedGrant: router.Handler(standalonehandlers.MerchantRevokeFederatedGrant(s.controlPlane)),
		}
		httproutes.RegisterControlPlaneRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordMerchantRoute), s.runtime, control)
	}

	httproutes.RegisterStaffRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordMerchantRoute), s.runtime, opts)
}

func (s *Server) registerMerchantActionRoutes(mux router.Registrar) {
	s.registerMerchantActionRoutesAt(mux, StandaloneV1Prefix)
}
