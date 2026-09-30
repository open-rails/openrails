package server

import (
	"github.com/open-rails/openrails/internal/http/router"

	log "github.com/sirupsen/logrus"
)

// authAPIBase is where the control plane's AuthKit JSON API lives (#224):
// /auth beneath an origin issuer, else the issuer's path.
func (s *Server) authAPIBase() string { return s.controlPlane.AuthAPIBase() }

// registerControlPlaneAuthRoutes mounts the AuthKit route groups OpenRails
// intentionally exposes (#224 task 4): the posture's explicit group list
// (ControlPlane.MountedRouteGroups, never AuthKit's default surface or browser
// OIDC), plus JWKS at the issuer.
func (s *Server) registerControlPlaneAuthRoutes(mux router.Registrar) error {
	cp := s.controlPlane
	if cp == nil || cp.AuthHandler() == nil {
		return nil
	}
	routes := cp.AuthRoutes()
	for _, route := range routes {
		s.handle(mux, route.Pattern(), cp.AuthHandler())
	}
	log.WithFields(log.Fields{
		"routes":      len(routes),
		"self_hosted": cp.SelfHostedPosture(),
	}).Info("control plane: mounted selective AuthKit route groups")
	return nil
}
