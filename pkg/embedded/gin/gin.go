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
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	internalauth "github.com/open-rails/openrails/internal/auth"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/internal/http/router/ginrouter"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// RouteOptions configures route registration behavior.
type RouteOptions struct {
	// AuthProvider is required for routes that need authentication.
	AuthProvider ginauth.Provider
	// Gate protects merchant route groups. When nil, a gate is built from
	// AuthProvider plus the attached control plane when available.
	Gate billingauth.Gate
	// DelegatedAuthenticator protects delegated merchant/self-service requests
	// when registering route groups directly.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
}

// StandaloneHandler returns the full standalone gin HTTP surface for the embedded app:
// health + debug (dev only) + user + merchant + webhooks + API-key
// service routes. It is built from the gin-free app graph and is intended for
// the standalone server entrypoint (cmd/openrails).
func StandaloneHandler(e *embedded.Embedded) (http.Handler, error) {
	srv, err := newServer(e)
	if err != nil {
		return nil, err
	}
	return srv.Handler(), nil
}

// Handler returns the full standalone gin HTTP surface.
//
// Deprecated: use StandaloneHandler. Embedded hosts should usually use
// pkg/embedded.NewHTTPHandler or embed.Runtime.Handler instead.
func Handler(e *embedded.Embedded) (http.Handler, error) {
	return StandaloneHandler(e)
}

// RegisterUserRoutes registers user-facing billing routes on the provided gin
// router group. These routes include products, prices, checkout, subscriptions,
// payments, etc.
//
// Example:
//
//	router := gin.Default()
//	api := router.Group("/billing/v1")
//	embgin.RegisterUserRoutes(openrails, api, embgin.RouteOptions{})
func RegisterUserRoutes(e *embedded.Embedded, group *gin.RouterGroup, opts RouteOptions) {
	a := e.App()
	if a == nil {
		panic("embedded billing: not initialized")
	}
	authn := routeAuthenticator(opts)
	if authn == nil {
		panic("embedded billing: user routes require RouteOptions.AuthProvider")
	}
	httproutes.RegisterUserRoutes(ginrouter.New(group, a.Runtime), a.Runtime, httproutes.Options{
		Authenticator: authn,
	})
}

// RegisterMerchantActionRoutes registers merchant-scoped administrative action
// routes such as catalog product/price mutation. These routes require the
// narrow merchant:catalog:update permission rather than a broad owner grant.
func RegisterMerchantActionRoutes(e *embedded.Embedded, group *gin.RouterGroup, opts RouteOptions) {
	a := e.App()
	if a == nil {
		panic("embedded billing: not initialized")
	}
	authn := routeAuthenticator(opts)
	if authn == nil && opts.Gate == nil && opts.DelegatedAuthenticator == nil {
		panic("embedded billing: merchant action routes require RouteOptions.AuthProvider, RouteOptions.Gate, or RouteOptions.DelegatedAuthenticator")
	}
	actionOpts := httproutes.Options{
		Gate: opts.Gate,
	}
	if actionOpts.Gate == nil {
		actionOpts.Gate = httproutes.NewGate(httproutes.GateOptions{Authenticator: authn})
	}
	if cp := embcp.Get(a); cp != nil {
		actionOpts.Gate = httproutes.NewGate(httproutes.GateOptions{
			Authenticator:             authn,
			AdminPermissionChecker:    cp,
			ServiceCredentialResolver: cp,
			DelegatedResolver:         cp,
			DelegatedAuthenticator:    opts.DelegatedAuthenticator,
		})
	}
	httproutes.RegisterMerchantActionRoutes(ginrouter.New(group, a.Runtime), a.Runtime, actionOpts)
}

// RegisterWebhookRoutes registers legacy configured-merchant webhook routes.
// Embedded hosts should usually use RegisterMerchantWebhookRoutes or
// RegisterHostWebhookRoutes.
//
// Example:
//
//	router := gin.Default()
//	webhooks := router.Group("/billing/v1/webhooks")
//	embgin.RegisterWebhookRoutes(openrails, webhooks)
func RegisterWebhookRoutes(e *embedded.Embedded, group *gin.RouterGroup) {
	a := e.App()
	if a == nil {
		panic("embedded billing: not initialized")
	}
	httproutes.RegisterWebhookRoutes(ginrouter.New(group, a.Runtime), a.Runtime)
}

// RegisterHostWebhookRoutes registers POST /:provider on a group that already
// has host middleware pinning merchant.ID on the request context.
func RegisterHostWebhookRoutes(e *embedded.Embedded, group *gin.RouterGroup) {
	a := e.App()
	if a == nil {
		panic("embedded billing: not initialized")
	}
	httproutes.RegisterHostWebhookRoutes(ginrouter.New(group, a.Runtime), a.Runtime)
}

// routeAuthenticator resolves the framework-neutral Authenticator for a gin
// route registration from the per-call RouteOptions.AuthProvider.
func routeAuthenticator(opts RouteOptions) billingauth.Authenticator {
	if opts.AuthProvider != nil {
		if a, ok := ginauth.AsAuthenticator(opts.AuthProvider); ok {
			return a
		}
	}
	return nil
}

// newServer constructs the gin billing server graph from the embedded app.
//
// This is the standalone surface, so it wires the OpenRails-owned control plane
// (#284/#469) and uses that control plane's AuthKit verifier when no host
// authenticator was injected. There is no config-declared verifier-only mode.
func newServer(e *embedded.Embedded) (*server.Server, error) {
	a := e.App()
	if a == nil {
		return nil, fmt.Errorf("embedded billing: not initialized")
	}
	cfg := a.Config
	if embcp.Get(a) == nil {
		if err := embcp.Attach(context.Background(), a, cfg, nil); err != nil {
			return nil, fmt.Errorf("attach control plane: %w", err)
		}
	}
	authenticator := func() billingauth.Authenticator {
		cp := embcp.Get(a)
		if cp == nil || cp.AuthService() == nil || cp.AuthService().Verifier() == nil {
			return nil
		}
		return internalauth.NewAuthenticator(cp.AuthService().Verifier())
	}()
	if authenticator == nil {
		return nil, fmt.Errorf("control plane verifier unavailable")
	}
	return server.New(server.Dependencies{
		Config:             a.Config,
		Cache:              a.Cache,
		Runtime:            a.Runtime,
		Redis:              a.RedisClient,
		Authenticator:      authenticator,
		ConfiguredMerchant: a.Runtime.ConfiguredMerchant,
		ControlPlane:       embcp.Get(a),
	})
}
