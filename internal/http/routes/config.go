package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/router"
)

// configRoutes serve the public configuration: what the mount serves, the
// currency registry and the merchant's browser payment setup. The public
// read is always mounted, so even a minimal deployment is discoverable; the
// merchant reads the same document.
var configRoutes = []Route{
	{Method: GET, Path: "/v1/config", Group: Meta, Auth: AuthPublic,
		Responses: []Reply{{200, billing.PublicConfig{}}}, Bind: publicConfig},
	{Method: GET, Path: "/v1/admin/config", Group: Admin, Auth: AuthMerchant, Name: "GetPublicConfig", Level: LevelRead,
		Responses: []Reply{{200, billing.PublicConfig{}}}, Errors: codes("service_unavailable"), Bind: merchantConfig},
}

// publicConfig binds the public read to the capabilities its assembly
// supplies; without them it is not mounted.
func publicConfig(e *Env) router.Handler {
	if e.Capabilities == nil {
		return nil
	}
	return router.Handler(handlers.GetPublicConfig(*e.Capabilities))
}

// merchantConfig binds the merchant's read, which every staff mount serves.
func merchantConfig(e *Env) router.Handler {
	caps := billing.Capabilities{RouteGroups: map[string]bool{}, Features: map[string]bool{}}
	if e.Capabilities != nil {
		caps = *e.Capabilities
	}
	return router.Handler(handlers.ServiceGetPublicConfig(caps))
}
