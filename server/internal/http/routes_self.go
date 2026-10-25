package server

import (
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerSelfServiceRoutes mounts the customer surface under /v1/me/*: a
// customer's credential is bound to its merchant's group, calling OpenRails
// directly from the browser.
func (s *Server) registerSelfServiceRoutes(mux router.Registrar) {
	httproutes.RegisterCustomerRoutes(
		router.NewMuxRecorded(mux, StandaloneV1Prefix+httproutes.SelfRoutePrefix, s.runtime, s.recordBrowserRoute),
		s.runtime, httproutes.CustomerMount{Auth: s.auth, AdmissionPolicy: s.customerAdmissionPolicy, Providers: embedhttp.ProviderRoutesForRuntime(s.runtime, nil)})

	log.WithField("prefix", StandaloneV1Prefix+httproutes.SelfRoutePrefix).
		Info("self-service API routes registered on public handler")
}
