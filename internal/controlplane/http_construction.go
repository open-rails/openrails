package controlplane

import (
	"net/http"
	"strings"

	authhttp "github.com/open-rails/authkit/authhttp"
	authcore "github.com/open-rails/authkit/embedded"
	riverhelpers "github.com/open-rails/helpers/river"
)

// The private HTTP constructor is the only place where local protocol
// capabilities cross the AuthKit engine boundary. No engine is retained on the
// control-plane application Client.
type controlPlaneHTTP struct {
	controlPlane *ControlPlane
	config       authhttp.Config
	requestURL   func(*http.Request) string
}
type controlPlaneHTTPSurface struct {
	*authhttp.Service
	routes []authcore.HTTPRoute
}

func (s *controlPlaneHTTPSurface) Routes() []authcore.HTTPRoute {
	return append([]authcore.HTTPRoute(nil), s.routes...)
}
func (c controlPlaneHTTP) BuildHTTP(backend authcore.HTTPBackend) (authcore.HTTPSurface, error) {
	service, err := authhttp.New(backend, c.config)
	if err != nil {
		return nil, err
	}
	c.controlPlane.authSvc = service
	delegated, err := newDelegatedVerifier(backend, APIKeyPrefix, c.requestURL)
	if err != nil {
		service.Close()
		return nil, err
	}
	delegated.WithService(backend)
	c.controlPlane.delegatedVerifier = delegated
	surface := &controlPlaneHTTPSurface{Service: service}
	groups := c.controlPlane.MountedRouteGroups()
	if len(groups) == 0 {
		// AuthKit interprets an empty allow-list as its default surface.
		// OpenRails keeps an empty selection closed instead.
		return surface, nil
	}
	mount, err := authhttp.NewMount(service, authhttp.MountOptions{
		APIPrefix: "/auth", Groups: groups,
		ExcludeRoutes: []authhttp.RouteRef{{Method: http.MethodGet, Path: authhttp.JWKSPath}},
		Wrap:          c.controlPlane.WrapAuthRoute,
	})
	if err != nil {
		service.Close()
		return nil, err
	}
	for _, route := range mount.Routes() {
		if !strings.HasPrefix(route.Path, "/auth/") {
			continue
		}
		surface.routes = append(surface.routes, authcore.HTTPRoute{Method: route.Method, Path: route.Path, Handler: mount})
	}
	return surface, nil
}

func (c *ControlPlane) RiverJobs() riverhelpers.Contribution { return c.authClient.RiverJobs() }

// AuthRoutes returns the runtime's already configured local HTTP inventory.
func (c *ControlPlane) AuthRoutes() ([]authcore.HTTPRoute, error) {
	if c == nil || c.authClient == nil {
		return nil, nil
	}
	return c.authClient.HTTPRoutes()
}
