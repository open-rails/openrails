package server

import (
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
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
		// #757 self-serve API keys (mint/list/revoke via AuthKit core).
		APIKeys: s.controlPlane,
		// #760 team management (roster/invites/role/remove via AuthKit membership).
		Team:         s.controlPlane,
		AdminLimiter: s.adminLimiter,
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
