// Package gin holds the gin-coupled surface of pkg/embedded (#285). The core
// pkg/embedded package is gin-free and exposes only the net/http NewHTTPHandler;
// hosts that mount billing onto an existing gin engine, or that want the full
// standalone gin handler, use the helpers here.
//
// Everything in this package is built from the gin-free *app.App that the core
// embedded.Embedded carries (accessed via Embedded.App()), so the gin dependency
// stays isolated to this subpackage.
package gin

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/internal/http/router/ginrouter"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
)

// RouteOptions configures route registration behavior.
type RouteOptions struct {
	// AuthProvider is required for routes that need authentication.
	// If not provided, uses the auth provider from Embedded initialization.
	AuthProvider ginauth.Provider
}

// Handler returns the full standalone gin HTTP surface for the embedded app:
// health + debug (dev only) + user + admin + webhooks + the OAT-authenticated
// server-to-server service routes. It is built from the gin-free app graph and
// is intended for the standalone server entrypoint (cmd/billing).
func Handler(e *embedded.Embedded) (http.Handler, error) {
	srv, err := newServer(e)
	if err != nil {
		return nil, err
	}
	return srv.Handler(), nil
}

// RegisterUserRoutes registers user-facing billing routes on the provided gin
// router group. These routes include products, prices, checkout, subscriptions,
// payments, etc.
//
// Example:
//
//	router := gin.Default()
//	api := router.Group("/billing/v1")
//	embgin.RegisterUserRoutes(billing, api, embgin.RouteOptions{})
func RegisterUserRoutes(e *embedded.Embedded, group *gin.RouterGroup, opts RouteOptions) {
	a := e.App()
	if a == nil {
		panic("embedded billing: not initialized")
	}
	httproutes.RegisterUserRoutes(ginrouter.New(group, a.Runtime), a.Runtime, httproutes.Options{
		Authenticator: routeAuthenticator(e, opts),
	})
}

// RegisterAdminRoutes registers admin billing routes on the provided gin router
// group. These routes include subscription management, payment management, user
// management, and metrics. All routes require admin authorization.
//
// Example:
//
//	router := gin.Default()
//	admin := router.Group("/billing/v1/admin")
//	embgin.RegisterAdminRoutes(billing, admin, embgin.RouteOptions{})
func RegisterAdminRoutes(e *embedded.Embedded, group *gin.RouterGroup, opts RouteOptions) {
	a := e.App()
	if a == nil {
		panic("embedded billing: not initialized")
	}
	httproutes.RegisterAdminRoutes(ginrouter.New(group, a.Runtime), a.Runtime, httproutes.Options{
		Authenticator: routeAuthenticator(e, opts),
	})
}

// RegisterWebhookRoutes registers webhook routes on the provided gin router
// group. These routes handle incoming webhooks from payment processors (Stripe,
// CCBill, NMI, etc.).
//
// Example:
//
//	router := gin.Default()
//	webhooks := router.Group("/billing/v1/webhooks")
//	embgin.RegisterWebhookRoutes(billing, webhooks)
func RegisterWebhookRoutes(e *embedded.Embedded, group *gin.RouterGroup) {
	a := e.App()
	if a == nil {
		panic("embedded billing: not initialized")
	}
	httproutes.RegisterWebhookRoutes(ginrouter.New(group, a.Runtime), a.Runtime)
}

// routeAuthenticator resolves the framework-neutral Authenticator for a gin
// route registration: the per-call RouteOptions.AuthProvider (a gin Provider)
// wins when it exposes one, else the app's configured authenticator (#282/#285).
func routeAuthenticator(e *embedded.Embedded, opts RouteOptions) billingauth.Authenticator {
	if opts.AuthProvider != nil {
		if a, ok := ginauth.AsAuthenticator(opts.AuthProvider); ok {
			return a
		}
	}
	if a := e.App(); a != nil {
		return a.Authenticator
	}
	return nil
}

// newServer constructs the gin billing server graph from the embedded app.
func newServer(e *embedded.Embedded) (*server.Server, error) {
	a := e.App()
	if a == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	return server.New(server.Dependencies{
		Config:        a.Config,
		Cache:         a.Cache,
		Runtime:       a.Runtime,
		Redis:         a.RedisClient,
		Authenticator: a.Authenticator,
		ControlPlane:  a.ControlPlane,
	})
}
