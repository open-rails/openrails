package server

import (
	"net/http"

	authhttp "github.com/open-rails/authkit/authhttp"
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
// group list. The lazy customer permission-group ensure rides
// MountOptions.Wrap.
func (s *Server) registerControlPlaneAuthRoutes(mux *http.ServeMux) error {
	cp := s.controlPlane
	if cp == nil || cp.AuthService() == nil {
		return nil
	}
	groups := cp.MountedRouteGroups()
	if len(groups) == 0 {
		// Fail closed: an empty allow-list mounts nothing, never the default surface.
		return nil
	}
	mount, err := authhttp.MountHandler(cp.AuthService(), authhttp.MountOptions{
		Groups:        groups,
		APIPrefix:     ControlPlaneAuthPrefix,
		ExcludeRoutes: []authhttp.RouteRef{{Method: http.MethodGet, Path: authhttp.JWKSPath}},
		Wrap:          cp.WrapAuthRoute,
	})
	if err != nil {
		return err
	}
	// Record the served surface per spec — the mount serves exactly
	// ControlPlane.RouteSpecs() under /auth (pinned by the route-surface test).
	specs := cp.RouteSpecs()
	for _, spec := range specs {
		s.recordRoute(spec.Method + " " + ControlPlaneAuthPrefix + spec.Path)
	}
	mux.Handle(ControlPlaneAuthPrefix+"/", mount)
	// Exact /auth: the mount's clean 404, not ServeMux's implicit 301 to /auth/.
	mux.Handle(ControlPlaneAuthPrefix, mount)
	log.WithFields(log.Fields{
		"prefix":      ControlPlaneAuthPrefix,
		"routes":      len(specs),
		"self_hosted": cp.SelfHostedPosture(),
	}).Info("control plane: mounted selective AuthKit route groups")
	return nil
}
