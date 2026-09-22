package server

import (
	"github.com/open-rails/openrails/internal/http/router"
	"net/http"
	"strings"

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
// group list. Hosted merchant creation's directory attachment rides
// MountOptions.Wrap; other requests never create portal groups.
func (s *Server) registerControlPlaneAuthRoutes(mux router.Registrar) error {
	cp := s.controlPlane
	if cp == nil || cp.AuthService() == nil {
		return nil
	}
	groups := cp.MountedRouteGroups()
	if len(groups) == 0 {
		// Fail closed: an empty allow-list mounts nothing, never the default surface.
		return nil
	}
	mount, err := authhttp.NewMount(cp.AuthService(), authhttp.MountOptions{
		Groups:        groups,
		APIPrefix:     ControlPlaneAuthPrefix,
		ExcludeRoutes: []authhttp.RouteRef{{Method: http.MethodGet, Path: authhttp.JWKSPath}},
		Wrap:          cp.WrapAuthRoute,
	})
	if err != nil {
		return err
	}
	specs := mount.Routes()
	for _, spec := range specs {
		if strings.HasPrefix(spec.Path, ControlPlaneAuthPrefix+"/") {
			s.handle(mux, spec.Method+" "+spec.Path, mount)
		}
	}
	log.WithFields(log.Fields{
		"prefix":      ControlPlaneAuthPrefix,
		"routes":      len(specs),
		"self_hosted": cp.SelfHostedPosture(),
	}).Info("control plane: mounted selective AuthKit route groups")
	return nil
}
