package server

import (
	"github.com/open-rails/openrails/internal/http/router"
)

// registerControlPlaneAuthRoutes mounts ControlPlane.AuthRoutes: the
// registration mode's explicit AuthKit groups (never AuthKit's default surface
// or browser OIDC), plus JWKS at the issuer.
func (s *Server) registerControlPlaneAuthRoutes(mux router.Registrar) error {
	cp := s.controlPlane
	if cp == nil || cp.AuthHandler() == nil {
		return nil
	}
	routes := cp.AuthRoutes()
	for _, route := range routes {
		s.handle(mux, route.Pattern(), cp.AuthHandler())
	}
	s.Log.Info("control plane: mounted selective AuthKit route groups", "routes", len(routes), "registration", cp.Registration())
	return nil
}
