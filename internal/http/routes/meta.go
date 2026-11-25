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

// ServiceSummary is GET /'s answer: the service, its version, whether
// /health/ready answers ready, and the paths of its surfaces.
type ServiceSummary struct {
	Service string        `json:"service"`
	Version string        `json:"version"`
	Status  ServiceStatus `json:"status"`
	Links   ServiceLinks  `json:"links"`
}

// ServiceStatus is /health/ready's answer, said as a status.
type ServiceStatus string

const (
	ServiceReady    ServiceStatus = "ready"
	ServiceNotReady ServiceStatus = "not_ready"
)

// ServiceLinks are the server's surfaces, as paths from its root. Console is
// null when the console is not mounted.
type ServiceLinks struct {
	API     string  `json:"api"`
	Config  string  `json:"config"`
	Console *string `json:"console"`
	Live    string  `json:"live"`
	Ready   string  `json:"ready"`
}

// metaRoutes is the process surface: the summary and health. They sit at the
// root, outside /v1, and only the standalone server mounts them.
var metaRoutes = []Route{
	// Summary runs Ready's checks too, and answers 200 whatever they say.
	{Method: GET, Path: "/", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, ServiceSummary{}}}, Bind: external(func(x *External) http.Handler { return x.Summary })},
	{Method: GET, Path: "/health/live", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Health{}}}, Bind: external(func(x *External) http.Handler { return x.Live })},
	// Ready runs Engine.Ready's checks; the failing dependency is logged, not
	// answered.
	{Method: GET, Path: "/health/ready", Group: Meta, Auth: AuthPublic, NoConn: true,
		Responses: []Reply{{200, Health{}}}, Errors: codes("service_unavailable"), Bind: external(func(x *External) http.Handler { return x.Ready })},
}
