package server

import (
	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/internal/http/router/ginrouter"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

func (s *Server) registerMerchantActionRoutesAt(e *gin.Engine, apiPrefix string) {
	group := e.Group(apiPrefix + "/merchant")
	opts := httproutes.Options{
		Gate: httproutes.NewGate(httproutes.GateOptions{
			Authenticator:             s.embeddedAuthenticator(),
			AdminPermissionChecker:    s.controlPlane,
			ServiceCredentialResolver: s.controlPlane,
			DelegatedResolver:         s.controlPlane,
			DelegatedAuthenticator:    s.delegatedAuthenticator,
		}),
	}
	// #555 HARD CUT: the merchant API surface is `/v1/merchant/*`. Standalone
	// mounts every merchant route set here: human admin/support, settings/catalog,
	// and the machine billing API.
	httproutes.RegisterMerchantActionRoutes(ginrouter.New(group, s.runtime), s.runtime, opts)
	httproutes.RegisterCatalogRoutes(ginrouter.New(group.Group("/catalog"), s.runtime), s.runtime, opts)
	httproutes.RegisterPaymentProviderRoutes(ginrouter.New(group.Group("/payment-providers"), s.runtime), s.runtime, opts)
	httproutes.RegisterServiceRoutes(ginrouter.New(group, s.runtime), s.runtime, opts)
}

func (s *Server) registerMerchantActionRoutesOn(e *gin.Engine) {
	s.registerMerchantActionRoutesAt(e, StandaloneV1Prefix)
}
