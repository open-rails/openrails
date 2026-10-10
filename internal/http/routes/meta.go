package routes

import (
	"net/http"
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

// metaRoutes is the process surface: health. They sit at the root, outside
// /v1, and only the standalone server mounts them.
var metaRoutes = []Route{
	{Method: GET, Path: "/health/live", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Health{}}}, Bind: external(func(x *External) http.Handler { return x.Live })},
	// Ready runs Client.Ready's checks; the failing dependency is logged, not
	// answered.
	{Method: GET, Path: "/health/ready", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Health{}}}, Errors: codes("service_unavailable"), Bind: external(func(x *External) http.Handler { return x.Ready })},
}
