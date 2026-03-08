package embedded

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/server"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/service"
)

type Options struct {
	Config       *config.Config
	DB           *sql.DB
	PGXPool      *pgxpool.Pool
	Redis        *redis.Client
	AuthProvider authprovider.Provider
	Cache        cache.Cache
}

type Embedded struct {
	app    *app.App
	server *server.Server
}

func New(opts Options) (*Embedded, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("config is required")
	}

	assembled, err := bootstrap.NewServer(opts.Config, &bootstrap.Options{
		DB:           opts.DB,
		PGXPool:      opts.PGXPool,
		Redis:        opts.Redis,
		AuthProvider: opts.AuthProvider,
		Cache:        opts.Cache,
	})
	if err != nil {
		return nil, err
	}

	return &Embedded{
		app:    assembled.App,
		server: assembled.Server,
	}, nil
}

func (e *Embedded) Handler() http.Handler {
	if e == nil || e.server == nil {
		return nil
	}
	return e.server.Handler()
}

// HTTPHandlerOptions controls which billing HTTP route groups are included in the returned handler.
//
// If all fields are false (zero value), the options default to user + admin + webhooks.
//
// Note: billing health endpoints are not exposed in embedded mode.
// If a host wants billing readiness, call IsBillingReady and include it in the host's /readyz.
type HTTPHandlerOptions struct {
	IncludeUser     bool
	IncludeAdmin    bool
	IncludeWebhooks bool
}

// NewHTTPHandler returns a single mountable `http.Handler` for the selected route groups.
//
// Embedded routes live under `/billing/v1/*`.
func (e *Embedded) NewHTTPHandler(opts HTTPHandlerOptions) http.Handler {
	if e == nil || e.server == nil {
		return nil
	}
	return e.server.NewHTTPHandler(server.HTTPHandlerOptions{
		IncludeUser:     opts.IncludeUser,
		IncludeAdmin:    opts.IncludeAdmin,
		IncludeWebhooks: opts.IncludeWebhooks,
	})
}

// UserHandler exposes user/public billing APIs (and health endpoints).
// Mount this under a prefix like `/billing`.
// ServiceHandler returns the internal service-to-service HTTP API (X-API-KEY protected).
// Embedded hosts should typically NOT mount this; use `Embedded.Service()` instead.
func (e *Embedded) ServiceHandler() http.Handler {
	if e == nil || e.server == nil {
		return nil
	}
	return e.server.PrivateHandler()
}

// Service returns the in-process billing API for embedded hosts.
func (e *Embedded) Service() (*service.Service, error) {
	if e == nil || e.app == nil {
		return nil, fmt.Errorf("embedded billing: app not initialized")
	}
	return service.New(e.app.Runtime)
}

// RouteOptions configures route registration behavior.
type RouteOptions struct {
	// AuthProvider is required for routes that need authentication.
	// If not provided, uses the auth provider from Embedded initialization.
	AuthProvider authprovider.Provider
}

// RegisterUserRoutes registers user-facing billing routes on the provided Gin router group.
// These routes include products, prices, checkout, subscriptions, payments, etc.
//
// Example:
//
//	router := gin.Default()
//	api := router.Group("/billing/v1")
//	billing.RegisterUserRoutes(api, embedded.RouteOptions{})
func (e *Embedded) RegisterUserRoutes(group *gin.RouterGroup, opts RouteOptions) {
	if e == nil || e.app == nil {
		panic("embedded billing: not initialized")
	}
	auth := opts.AuthProvider
	if auth == nil {
		auth = e.app.AuthProvider
	}
	httproutes.RegisterUserRoutes(group, e.app.Runtime, httproutes.Options{
		AuthProvider: auth,
	})
}

// RegisterAdminRoutes registers admin billing routes on the provided Gin router group.
// These routes include subscription management, payment management, user management, and metrics.
// All routes require admin authorization.
//
// Example:
//
//	router := gin.Default()
//	admin := router.Group("/billing/v1/admin")
//	billing.RegisterAdminRoutes(admin, embedded.RouteOptions{})
func (e *Embedded) RegisterAdminRoutes(group *gin.RouterGroup, opts RouteOptions) {
	if e == nil || e.app == nil {
		panic("embedded billing: not initialized")
	}
	auth := opts.AuthProvider
	if auth == nil {
		auth = e.app.AuthProvider
	}
	httproutes.RegisterAdminRoutes(group, e.app.Runtime, httproutes.Options{
		AuthProvider: auth,
	})
}

// RegisterWebhookRoutes registers webhook routes on the provided Gin router group.
// These routes handle incoming webhooks from payment processors (Stripe, CCBill, NMI, etc.).
//
// Example:
//
//	router := gin.Default()
//	webhooks := router.Group("/billing/v1/webhooks")
//	billing.RegisterWebhookRoutes(webhooks)
func (e *Embedded) RegisterWebhookRoutes(group *gin.RouterGroup) {
	if e == nil || e.app == nil {
		panic("embedded billing: not initialized")
	}
	httproutes.RegisterWebhookRoutes(group, e.app.Runtime)
}

func (e *Embedded) RunWorkers(ctx context.Context) error {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return fmt.Errorf("runtime is not initialized")
	}
	return e.app.Runtime.RunWorkers(ctx)
}

func (e *Embedded) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	if e.server != nil {
		_ = e.server.Close(ctx)
	}
	if e.app != nil {
		return e.app.Close(ctx)
	}
	return nil
}

func (e *Embedded) Config() *config.Config {
	if e == nil || e.app == nil {
		return nil
	}
	return e.app.Config
}
