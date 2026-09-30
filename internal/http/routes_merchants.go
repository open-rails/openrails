package server

import (
	"net/http"

	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/standalonehandlers"
)

// registerMerchantAccountRoutes mounts /v1/merchants, a signed-in user's own
// merchants (#1106). Users create merchants only under a declared hosted
// creation policy, behind a per-IP and per-user velocity limit.
func (s *Server) registerMerchantAccountRoutes(mux router.Registrar) {
	if s.controlPlane == nil {
		return
	}
	user := httproutes.Options{Authenticator: s.authenticator}.RequireUser()
	rr := router.NewMuxRecorded(mux, StandaloneV1Prefix+"/merchants", s.runtime, s.recordRoute)
	if s.controlPlane.MerchantCreationEnabled() {
		rr.Handle(http.MethodPost, "", router.Handler(standalonehandlers.MerchantCreate(s.controlPlane)),
			user, middleware.VelocityLimit(middleware.MerchantCreationVelocity, s.rdb, s.trustedProxies()))
	}
}
