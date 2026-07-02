package server

import (
	"net/http"

	log "github.com/sirupsen/logrus"
)

// ControlPlaneAuthPrefix is where OpenRails mounts the selected AuthKit route
// groups (#224). It is a deliberate, narrow surface — NOT AuthKit DefaultAPI.
const ControlPlaneAuthPrefix = "/auth"

// registerControlPlaneAuthRoutes selectively mounts the AuthKit route groups
// OpenRails intentionally exposes (#224 task 4). In locked-down/self-hosted mode
// this is only the login/session/user groups; AuthKit DefaultAPI() is never
// mounted. AuthKit specs are native net/http ServeMux patterns, so they mount
// directly (#670 — the gin path translation shim is gone).
func (s *Server) registerControlPlaneAuthRoutes(mux *http.ServeMux) {
	specs := s.controlPlane.RouteSpecs()
	for _, spec := range specs {
		s.handle(mux, spec.Method+" "+ControlPlaneAuthPrefix+spec.Path, spec.Handler)
	}
	log.WithFields(log.Fields{
		"prefix":      ControlPlaneAuthPrefix,
		"routes":      len(specs),
		"self_hosted": s.controlPlane.SelfHostedPosture(),
	}).Info("control plane: mounted selective AuthKit route groups")
}
