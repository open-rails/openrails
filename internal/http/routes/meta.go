package routes

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// metaRoutes is the process surface: health, metrics, and what this
// deployment serves. Health and metrics sit at the root, outside /v1, and
// only the standalone server mounts them.
var metaRoutes = []Route{
	{Method: GET, Path: "/", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Bind: external(func(x *External) http.Handler { return x.Banner })},
	{Method: GET, Path: "/health/live", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Bind: external(func(x *External) http.Handler { return x.Live })},
	{Method: GET, Path: "/health/ready", Group: Meta, Auth: AuthPublic, NoConn: true,
		Query: params(text("verbose")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("service_unavailable"), Bind: external(func(x *External) http.Handler { return x.Ready })},
	// Kubernetes-style aliases.
	{Method: GET, Path: "/healthz", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Bind: external(func(x *External) http.Handler { return x.Live })},
	{Method: GET, Path: "/readyz", Group: Meta, Auth: AuthPublic, NoConn: true,
		Query: params(text("verbose")), Responses: []Reply{{200, Untyped{}}}, Errors: codes("service_unavailable"), Bind: external(func(x *External) http.Handler { return x.Ready })},
	{Method: GET, Path: "/metrics", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Stream{"text/plain"}}}, Bind: external(func(x *External) http.Handler { return x.Metrics })},
	// Capability discovery (#623): which route groups and provider actions
	// this deployment serves. Always on, so even a minimal deployment is
	// discoverable.
	{Method: GET, Path: "/v1/capabilities", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Untyped{}}}, Bind: external(func(x *External) http.Handler { return x.Capabilities })},
	// The currency scale registry behind every monetary string on the wire:
	// system-fixed, so it needs neither a merchant nor a database connection.
	{Method: GET, Path: "/v1/currencies", Group: Checkout, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, billing.CurrencyRegistry{}}}, Handler: h(handlers.GetCurrencies)},
}
