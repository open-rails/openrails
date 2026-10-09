package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/router"
)

// configRoutes serve the public configuration: what the mount serves, the
// currency registry and the merchant's browser payment setup. It is always
// mounted, so even a minimal deployment is discoverable.
var configRoutes = []Route{
	{Method: GET, Path: "/v1/config", Group: Meta, Auth: AuthPublic,
		Responses: []Reply{{200, billing.PublicConfig{}}}, Bind: publicConfig},
}

// publicConfig binds the public read to the capabilities its assembly
// supplies; without them it is not mounted.
func publicConfig(e *Env) router.Handler {
	if e.Capabilities == nil {
		return nil
	}
	return router.Handler(handlers.GetPublicConfig(*e.Capabilities))
}
