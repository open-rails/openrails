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
	"github.com/open-rails/openrails/internal/controlplane"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/internal/http/router/ginrouter"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/service"
)

type Options struct {
	Config  *config.Config
	DB      *sql.DB
	PGXPool *pgxpool.Pool
	Redis   *redis.Client
	// Authenticator is the framework-neutral auth boundary (gin-free). A host
	// brings its own auth by implementing billingauth.Authenticator. When nil, the
	// default AuthKit-backed authenticator is built from Config. Hosts that have a
	// gin Provider can adapt it with ginauth.AsAuthenticator (#285).
	Authenticator billingauth.Authenticator
	Cache         cache.Cache
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
		DB:            opts.DB,
		PGXPool:       opts.PGXPool,
		Redis:         opts.Redis,
		Authenticator: opts.Authenticator,
		Cache:         opts.Cache,
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

// Service returns the in-process billing API for embedded hosts.
func (e *Embedded) Service() (*service.Service, error) {
	if e == nil || e.app == nil {
		return nil, fmt.Errorf("embedded billing: app not initialized")
	}
	return service.New(e.app.Runtime)
}

// RunControlPlaneBootstrap idempotently bootstraps the OpenRails-owned AuthKit
// control plane (#224): operator org, OpenRails roles, the openrails.*
// permission catalog, and an initial operator OAT for the default tenant. It is
// a no-op when the control plane is disabled (verifier-only mode). Run it after
// migrations and at startup; safe to re-run.
func (e *Embedded) RunControlPlaneBootstrap(ctx context.Context, opts controlplane.BootstrapOptions) (*controlplane.BootstrapResult, error) {
	if e == nil || e.app == nil {
		return nil, fmt.Errorf("embedded billing: app not initialized")
	}
	return e.app.RunControlPlaneBootstrap(ctx, opts)
}

// ControlPlane returns the OpenRails-owned AuthKit control plane, or nil when it
// is disabled (verifier-only mode). Used for selective AuthKit route mounting.
func (e *Embedded) ControlPlane() *controlplane.ControlPlane {
	if e == nil || e.app == nil {
		return nil
	}
	return e.app.ControlPlane
}

// RouteOptions configures route registration behavior.
type RouteOptions struct {
	// AuthProvider is required for routes that need authentication.
	// If not provided, uses the auth provider from Embedded initialization.
	AuthProvider ginauth.Provider
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
	httproutes.RegisterUserRoutes(ginrouter.New(group, e.app.Runtime), e.app.Runtime, httproutes.Options{
		Authenticator: e.routeAuthenticator(opts),
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
	httproutes.RegisterAdminRoutes(ginrouter.New(group, e.app.Runtime), e.app.Runtime, httproutes.Options{
		Authenticator: e.routeAuthenticator(opts),
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
	httproutes.RegisterWebhookRoutes(ginrouter.New(group, e.app.Runtime), e.app.Runtime)
}

// routeAuthenticator resolves the framework-neutral Authenticator for a gin
// route registration: the per-call RouteOptions.AuthProvider (a gin Provider)
// wins when it exposes one, else the app's configured authenticator (#282/#285).
func (e *Embedded) routeAuthenticator(opts RouteOptions) billingauth.Authenticator {
	if opts.AuthProvider != nil {
		if a, ok := ginauth.AsAuthenticator(opts.AuthProvider); ok {
			return a
		}
	}
	if e != nil && e.app != nil {
		return e.app.Authenticator
	}
	return nil
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
