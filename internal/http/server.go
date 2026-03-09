package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/middleware"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/open-rails/openrails/pkg/cache"
)

type Dependencies struct {
	Config       *config.Config
	Cache        cache.Cache
	Runtime      *app.Runtime
	Redis        *redis.Client
	AuthProvider authprovider.Provider
}

type Server struct {
	cfg          *config.Config
	cache        cache.Cache
	runtime      *app.Runtime
	rdb          *redis.Client
	authProvider authprovider.Provider

	// publicHandler is the default "full surface" HTTP handler.
	// It includes health + debug (dev only) + user + admin + webhook routes.
	publicHandler *gin.Engine

	// privateHandler is the service-to-service API (X-API-KEY auth).
	// This runs on a separate port and should only be accessible within the Docker network.
	privateHandler *gin.Engine
}

func New(deps Dependencies) (*Server, error) {
	if deps.Config == nil {
		return nil, fmt.Errorf("server config is required")
	}
	if deps.Runtime == nil {
		return nil, fmt.Errorf("server runtime is required")
	}
	if deps.Runtime.DB == nil {
		return nil, fmt.Errorf("server runtime DB is required")
	}
	if deps.Runtime.Clock == nil {
		return nil, fmt.Errorf("server runtime clock is required")
	}
	if deps.Runtime.PaymentService == nil {
		return nil, fmt.Errorf("server runtime payment service is required")
	}
	if deps.Runtime.CheckoutService == nil {
		return nil, fmt.Errorf("server runtime checkout service is required")
	}
	if deps.Runtime.CheckoutSessionService == nil {
		return nil, fmt.Errorf("server runtime checkout session service is required")
	}
	if deps.Runtime.SubscriptionService == nil {
		return nil, fmt.Errorf("server runtime subscription service is required")
	}
	if deps.Runtime.UserSubscriptionService == nil {
		return nil, fmt.Errorf("server runtime user subscription service is required")
	}
	if deps.Runtime.PublicSubscriptionService == nil {
		return nil, fmt.Errorf("server runtime public subscription service is required")
	}
	if deps.Runtime.AdminSubscriptionService == nil {
		return nil, fmt.Errorf("server runtime admin subscription service is required")
	}
	if deps.Runtime.PaymentMethodService == nil {
		return nil, fmt.Errorf("server runtime payment method service is required")
	}
	if deps.Runtime.VaultService == nil {
		return nil, fmt.Errorf("server runtime vault service is required")
	}
	if deps.Runtime.ProcessorCustomerService == nil {
		return nil, fmt.Errorf("server runtime processor customer service is required")
	}
	if deps.Runtime.RiverProducer == nil {
		return nil, fmt.Errorf("server runtime river producer is required")
	}
	if deps.Cache == nil {
		return nil, fmt.Errorf("server cache is required")
	}
	if deps.AuthProvider == nil {
		return nil, fmt.Errorf("auth provider is required")
	}

	s := &Server{
		cfg:          deps.Config,
		cache:        deps.Cache,
		runtime:      deps.Runtime,
		rdb:          deps.Redis,
		authProvider: deps.AuthProvider,
	}

	s.setupPrivateHandler()

	// Default (standalone-friendly) HTTP surface.
	// Standalone mode owns service-level health/debug routes.
	s.publicHandler = s.newPublicEngine()
	s.registerStandaloneMetaRoutes(s.publicHandler)
	s.registerDebugRoutes(s.publicHandler)
	// Canonical: /v1/*
	s.registerUserRoutes(s.publicHandler)
	s.registerAdminRoutesOn(s.publicHandler)
	s.registerWebhookRoutes(s.publicHandler)

	// Private/service API surface.
	s.registerServiceRoutes()

	log.Info("Billing service initialized successfully")
	return s, nil
}

func (s *Server) newPublicEngine() *gin.Engine {
	e := gin.New()
	e.Use(gin.Recovery())
	e.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		SkipPaths: []string{"/health/live", "/health/ready", "/healthz", "/readyz", "/health"},
	}))
	e.Use(middleware.CORS(s.cfg.CorsOrigins))
	e.Use(middleware.RateLimit(s.cfg.RateLimits, s.rdb))
	return e
}

func (s *Server) setupPrivateHandler() {
	s.privateHandler = gin.New()
	s.privateHandler.Use(gin.Recovery())
	s.privateHandler.Use(gin.Logger())
	// No CORS needed for internal service-to-service calls.
}

func (s *Server) newHTTPHandlerEngine(opts HTTPHandlerOptions) *gin.Engine {
	opts = opts.withDefaults()
	e := s.newPublicEngine()

	if opts.IncludeUser {
		s.registerUserRoutesAt(e, EmbeddedV1Prefix)
	}
	if opts.IncludeAdmin {
		s.registerAdminRoutesAt(e, EmbeddedV1Prefix)
	}
	if opts.IncludeWebhooks {
		s.registerWebhookRoutesAt(e, EmbeddedV1Prefix)
	}
	return e
}

// NewHTTPHandler returns a single mountable `http.Handler` for the selected route groups.
//
// Intended for embedded hosts.
//
// Embedded routes live under `/billing/v1/*`.
func (s *Server) NewHTTPHandler(opts HTTPHandlerOptions) http.Handler {
	return s.newHTTPHandlerEngine(opts)
}

func (s *Server) wrap(fn func(r *httprequest.Request)) func(c *gin.Context) {
	return func(c *gin.Context) {
		fn(httprequest.New(c, s.runtime))
	}
}

// Handler returns the full public HTTP surface (health + debug (dev only) + user + admin + webhooks).
// It is designed to be mounted at a path prefix via http.StripPrefix.
func (s *Server) Handler() http.Handler { return s.publicHandler }

// PrivateHandler returns the internal service-to-service HTTP API (X-API-KEY protected).
func (s *Server) PrivateHandler() http.Handler { return s.privateHandler }

// Close currently does not own underlying resources; callers should close the App.
func (s *Server) Close(_ context.Context) error {
	log.Info("Billing HTTP server shut down")
	return nil
}

func (s *Server) Cfg() *config.Config {
	return s.cfg
}
