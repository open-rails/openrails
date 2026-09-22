package server

import (
	"net/http"

	log "github.com/sirupsen/logrus"
)

// ControlPlaneAuthPrefix is where OpenRails mounts the selected AuthKit route
// groups (#224). It is a deliberate, narrow surface — NOT AuthKit DefaultAPI.
const ControlPlaneAuthPrefix = "/auth"

// registerControlPlaneAuthRoutes mounts the AuthKit route groups OpenRails
// intentionally exposes (#224 task 4) as ONE neutral authhttp.MountHandler
// (authkit #250) under /auth. Groups is the posture's explicit allow-list
// (ControlPlane.MountedRouteGroups — never nil, which would mount AuthKit's
// default surface plus browser OIDC); JWKS is excluded (OpenRails does not
// expose AuthKit's JWKS on this surface) and browser OIDC is in no mounted
// group list. Hosted merchant creation's directory attachment rides
// MountOptions.Wrap; other requests never create portal groups.
func (s *Server) registerControlPlaneAuthRoutes(mux *http.ServeMux) error {
	cp := s.controlPlane
	if cp == nil || cp.AuthService() == nil {
		return nil
	}
	routes, err := cp.AuthRoutes()
	if err != nil {
		return err
	}
	for _, route := range routes {
		s.recordRoute(route.Method + " " + route.Path)
		mux.Handle(route.Method+" "+route.Path, route.Handler)
	}

	log.WithFields(log.Fields{
		"prefix":      ControlPlaneAuthPrefix,
		"routes":      len(routes),
		"self_hosted": cp.SelfHostedPosture(),
	}).Info("control plane: mounted selective AuthKit route groups")
	return nil
}
