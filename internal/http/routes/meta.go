package routes

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// Health is a health route's answer.
type Health struct {
	Status HealthStatus `json:"status"`
}

// HealthStatus is HealthOK from /health/live and HealthReady from
// /health/ready.
type HealthStatus string

const (
	HealthOK    HealthStatus = "ok"
	HealthReady HealthStatus = "ready"
)

// metaRoutes is the process surface: health, metrics, and what this
// deployment serves. Health and metrics sit at the root, outside /v1, and
// only the standalone server mounts them.
var metaRoutes = []Route{
	{Method: GET, Path: "/health/live", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Health{}}}, Bind: external(func(x *External) http.Handler { return x.Live })},
	// Ready runs Client.Ready's checks; the failing dependency is logged, not
	// answered.
	{Method: GET, Path: "/health/ready", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Health{}}}, Errors: codes("service_unavailable"), Bind: external(func(x *External) http.Handler { return x.Ready })},
	{Method: GET, Path: "/metrics", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Stream{"text/plain"}}}, Bind: external(func(x *External) http.Handler { return x.Metrics })},
	// Capability discovery (#623): which route groups and provider actions
	// this deployment serves. Always on, so even a minimal deployment is
	// discoverable.
	{Method: GET, Path: "/v1/capabilities", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, billing.Capabilities{}}}, Bind: external(func(x *External) http.Handler { return x.Capabilities })},
	// The currency scale registry behind every monetary string on the wire:
	// system-fixed, so it needs neither a merchant nor a database connection.
	{Method: GET, Path: "/v1/currencies", Group: Checkout, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, billing.CurrencyRegistry{}}}, Handler: h(handlers.GetCurrencies)},
}
