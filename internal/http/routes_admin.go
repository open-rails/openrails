package server

import (
	"github.com/gin-gonic/gin"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/http/router/ginrouter"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// #528: registerAdminRoutes{At,On} removed — the per-user `/v1/admin` surface is
// retired; the delegated admin surface is mounted by registerSelfServiceRoutes.

func (s *Server) registerMerchantActionRoutesAt(e *gin.Engine, apiPrefix string) {
	group := e.Group(apiPrefix + "/merchant")
	opts := httproutes.Options{
		Authenticator: s.embeddedAuthenticator(),
	}
	if s.controlPlane != nil {
		opts.AdminPermissionChecker = authpolicy.AdminPermissionChecker(s.controlPlane)
		opts.ServiceCredentialResolver = s.controlPlane
		// #555: standalone merchant action routes also accept a browser-direct
		// delegated merchant-admin token (the control plane resolves it).
		opts.DelegatedResolver = s.controlPlane
	}
	// #555 HARD CUT: the merchant API surface is `/v1/merchant/*`. The catalog
	// admin actions and the (formerly `/v1/service/*`) machine billing routes both
	// mount here, gated by `merchant:*` permissions; the old gin `/v1/service`
	// duplicate is deleted.
	httproutes.RegisterMerchantActionRoutes(ginrouter.New(group, s.runtime), s.runtime, opts)
	httproutes.RegisterServiceRoutes(ginrouter.New(group, s.runtime), s.runtime, opts)
}

func (s *Server) registerMerchantActionRoutesOn(e *gin.Engine) {
	s.registerMerchantActionRoutesAt(e, StandaloneV1Prefix)
}
