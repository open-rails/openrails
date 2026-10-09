package server

import (
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/standalonehandlers"
)

// registerMerchantAccountRoutes mounts /v1/merchants, a signed-in user's own
// merchants (#1106): GET lists them with the user's role; POST creates one, only
// under a declared hosted creation policy and behind a per-IP and per-user
// velocity limit.
func (s *Server) registerMerchantAccountRoutes(mux router.Registrar) {
	if s.controlPlane == nil {
		return
	}
	httproutes.RegisterControlPlaneRoutes(router.NewMuxRecorded(mux, StandaloneV1Prefix, s.runtime, s.recordRoute), s.runtime, httproutes.Options{
		Authenticator: s.authenticator,
		ResourceUsers: s.controlPlane,
		External: httproutes.External{
			ListMerchants:           router.Handler(standalonehandlers.MerchantListMine(s.controlPlane)),
			CreateMerchant:          router.Handler(standalonehandlers.MerchantCreate(s.controlPlane)),
			MerchantCreationEnabled: s.controlPlane.MerchantCreationEnabled(),
			MerchantCreationLimit:   middleware.VelocityLimit(middleware.MerchantCreationVelocity, s.rdb, s.trustedProxies()),
		},
	})
}
