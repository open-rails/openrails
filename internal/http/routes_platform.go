package server

import (
	"net/http"

	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// registerPlatformRoutes mounts the cross-merchant platform operator directory
// at /v1/platform/merchants (#721; openrails-saas #16). STANDALONE ONLY: the
// platform tier does not exist on the embedded surface (an embedded host
// controls exactly one merchant, and the control plane the root-group check
// needs only runs here).
func (s *Server) registerPlatformRoutes(mux *http.ServeMux) {
	opts := httproutes.PlatformOptions{
		Authenticator: s.authenticator,
		Root:          s.controlPlane,
		AdminLimiter:  s.adminLimiter,
	}
	httproutes.RegisterPlatformRoutes(
		router.NewMuxRecorded(mux, StandaloneV1Prefix+"/platform", s.runtime, s.recordRoute),
		s.runtime,
		opts,
	)
}
