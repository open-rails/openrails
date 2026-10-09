package server

import (
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/staffperm"
	"github.com/open-rails/openrails/internal/standalonehandlers"
)

// staffGuards are the server's own permissions on its merchant persona.
var staffGuards = map[httproutes.GuardKey]string{
	httproutes.StaffReadsKey:     staffperm.Read,
	httproutes.StaffWritesKey:    staffperm.Write,
	httproutes.MerchantConfigKey: staffperm.Admin,
}

func (s *Server) registerMerchantActionRoutesAt(mux router.Registrar, apiPrefix string) {
	opts := httproutes.Options{
		Auth:              s.staffAuth(),
		AuthBindsMerchant: true,
		AdminLimiter:      s.adminLimiter,
		Capabilities:      s.capabilities(),
	}
	guard, err := httproutes.ResolveGuards(httproutes.PlanStaffRoutes(s.runtime, opts, httproutes.Merchant, httproutes.MerchantConfig), staffGuards)
	if err != nil {
		panic(err)
	}
	opts.Guard = guard
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

	httproutes.RegisterMerchantRoutes(router.NewMuxRecorded(mux, apiPrefix, s.runtime, s.recordMerchantRoute), s.runtime, opts, httproutes.Merchant, httproutes.MerchantConfig)
}

func (s *Server) registerMerchantActionRoutes(mux router.Registrar) {
	s.registerMerchantActionRoutesAt(mux, StandaloneV1Prefix)
}
