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
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/crypto"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/platform"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/open-rails/openrails/pkg/cache"
)

type Dependencies struct {
	Config       *config.Config
	Cache        cache.Cache
	Runtime      *app.Runtime
	Redis        *redis.Client
	AuthProvider authprovider.Provider
	// ControlPlane is OpenRails' OpenRails-owned AuthKit control plane (#224).
	// nil in verifier-only mode. When present, the server selectively mounts the
	// intentional AuthKit route groups (never DefaultAPI in locked-down mode).
	ControlPlane *controlplane.ControlPlane
}

type Server struct {
	cfg          *config.Config
	cache        cache.Cache
	runtime      *app.Runtime
	rdb          *redis.Client
	authProvider authprovider.Provider
	controlPlane *controlplane.ControlPlane
	captchaStore *captcha.ChallengeStore

	// tenancy is the tenant provisioning + lifecycle + per-tenant secret service
	// (issue #225). Built only when the control plane is present (it owns the
	// billing.* control-plane pool and the operator-org provisioner). nil in
	// verifier-only mode: the tenant webhook + provisioning admin routes are then
	// not mounted and the single default tenant continues via the global webhook.
	tenancy *tenancy.Service

	// Platform superadmin layer (issue #226), DISTINCT from per-tenant operator
	// admin. Built only when the control plane is present (they share the
	// billing.* control-plane pool). platformAudit records every cross-tenant
	// superadmin mutation; platformBreakGlass manages time-boxed elevation;
	// platformMetrics aggregates platform-wide tenant metrics. nil in
	// verifier-only mode: the /v1/platform/* surface is then not mounted.
	platformAudit      *platform.AuditLog
	platformBreakGlass *platform.BreakGlass
	platformMetrics    *platform.Metrics

	// publicHandler is the single "full surface" HTTP handler. It includes
	// health + debug (dev only) + user + admin + webhook routes AND the
	// OAT-authenticated server-to-server service routes (issue #222). There is no
	// separate private/service trust surface or port.
	publicHandler *gin.Engine
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
		controlPlane: deps.ControlPlane,
		captchaStore: captcha.NewChallengeStore(deps.Redis),
	}

	// Build the tenant provisioning/lifecycle/secret service when the control
	// plane is present (issue #225). It reuses the control plane's pgx pool (the
	// OpenRails-owned billing.* control-plane DB) and operator-org provisioner. The
	// DB-backed secret store is the self-hosted default and needs no live Vault; a
	// managed deployment swaps in the Vault-backed store with the same addressing.
	if deps.ControlPlane != nil && deps.ControlPlane.Pool() != nil {
		var secretStore tenancy.TenantSecretStore
		var solanaTransit solanaint.TransitClient

		if deps.Config != nil && deps.Config.Vault != nil && deps.Config.Vault.Enabled {
			// Managed: Vault KV-v2 backend (#251), same (tenant, name) addressing.
			// Vault encrypts at rest, so no envelope wrapper. Optional Transit signer
			// keeps the per-tenant Solana key non-extractable.
			vc := deps.Config.Vault
			kvMount := vc.KVMount
			if kvMount == "" {
				kvMount = "secret"
			}
			vclient, verr := vault.Login(context.Background(), vault.Config{
				Address: vc.Address, AuthMethod: vc.AuthMethod, RoleID: vc.RoleID,
				SecretID: vc.SecretID, K8sRole: vc.K8sRole, KVMount: kvMount, TransitMount: vc.TransitMount,
			})
			if verr != nil {
				return nil, fmt.Errorf("vault login: %w", verr)
			}
			secretStore = tenancy.NewVaultSecretStore(kvMount, vault.NewKVv2Adapter(vclient, kvMount))
			if vc.UseTransitForSolana {
				tMount := vc.TransitMount
				if tMount == "" {
					tMount = "transit"
				}
				solanaTransit = vault.NewTransitAdapter(vclient, tMount)
			}
		} else {
			// Self-hosted default: DB-backed store + per-tenant envelope encryption
			// (issue #227). With no master key, the plain store is used unchanged.
			dbStore, sserr := tenancy.NewDBSecretStore(deps.ControlPlane.Pool())
			if sserr != nil {
				return nil, fmt.Errorf("build tenant secret store: %w", sserr)
			}
			var masterKey string
			if deps.Config != nil && deps.Config.Encryption != nil {
				masterKey = deps.Config.Encryption.MasterKey
			}
			dekStore, dkerr := crypto.NewDBDEKStore(deps.ControlPlane.Pool())
			if dkerr != nil {
				return nil, fmt.Errorf("build tenant DEK store: %w", dkerr)
			}
			enc, encerr := crypto.NewEncryptor(masterKey, dekStore)
			if encerr != nil {
				return nil, fmt.Errorf("build tenant encryptor: %w", encerr)
			}
			secretStore, sserr = tenancy.NewEncryptedSecretStore(dbStore, enc)
			if sserr != nil {
				return nil, fmt.Errorf("wrap tenant secret store with encryption: %w", sserr)
			}
		}

		tsvc, terr := tenancy.NewService(deps.ControlPlane.Pool(), deps.ControlPlane, secretStore)
		if terr != nil {
			return nil, fmt.Errorf("build tenancy service: %w", terr)
		}
		s.tenancy = tsvc

		// Recurring Solana cranker (#256): inject the per-tenant cranker BEFORE
		// workers start (InitRiver). Transit signer when configured (key never
		// leaves Vault), else a keypair signer over the secret store.
		if deps.Runtime != nil && deps.Runtime.SolanaRPC != nil {
			if solanaTransit != nil {
				deps.Runtime.SetSolanaCranker(recurring.NewCrankServiceFromTransit(solanaTransit, deps.Runtime.SolanaRPC, 0))
			} else {
				deps.Runtime.SetSolanaCranker(recurring.NewCrankServiceFromStore(secretStore, deps.Runtime.SolanaRPC, 0))
			}
		}

		// Platform superadmin layer (issue #226): cross-tenant audit, break-glass,
		// and platform metrics over the same control-plane pool.
		auditLog, aerr := platform.NewAuditLog(deps.ControlPlane.Pool())
		if aerr != nil {
			return nil, fmt.Errorf("build platform audit log: %w", aerr)
		}
		breakGlass, berr := platform.NewBreakGlass(deps.ControlPlane.Pool(), auditLog)
		if berr != nil {
			return nil, fmt.Errorf("build platform break-glass: %w", berr)
		}
		metrics, merr := platform.NewMetrics(deps.ControlPlane.Pool())
		if merr != nil {
			return nil, fmt.Errorf("build platform metrics: %w", merr)
		}
		s.platformAudit = auditLog
		s.platformBreakGlass = breakGlass
		s.platformMetrics = metrics
	}

	// Single (standalone-friendly) HTTP surface.
	// Standalone mode owns service-level health/debug routes.
	s.publicHandler = s.newPublicEngine()
	s.registerStandaloneMetaRoutes(s.publicHandler)
	s.registerDebugRoutes(s.publicHandler)
	// Canonical: /v1/*
	s.registerUserRoutes(s.publicHandler)
	s.registerAdminRoutesOn(s.publicHandler)
	s.registerWebhookRoutes(s.publicHandler)

	// Selective AuthKit route mounting (#224). In locked-down mode this mounts
	// ONLY the intentional AuthKit route groups (login/session/user) under
	// /auth — never AuthKit DefaultAPI. No-op in verifier-only mode.
	s.registerControlPlaneAuthRoutes(s.publicHandler)

	// Server-to-server service API: OAT-authenticated, on the SAME public engine
	// (issue #222). No private port, no mTLS listener. No-op without a control
	// plane (verifier-only mode has no OAT issuer).
	s.registerServiceRoutes(s.publicHandler)

	// Browser-direct self-service API: delegated-access-token-authenticated, on
	// the SAME public engine (issue #222 browser tier). No-op without a control
	// plane (verifier-only mode has no delegated-token issuer).
	s.registerSelfServiceRoutes(s.publicHandler)

	// Tenant-scoped webhook routing (issue #225): /v1/t/:tenant/webhooks/:provider
	// resolves the tenant from the path slug, then loads THAT tenant's signing
	// secret and verifies the signature AFTER tenant resolution. No-op without the
	// tenancy service (verifier-only mode).
	s.registerTenantWebhookRoutes(s.publicHandler)

	// Operator-gated tenant provisioning/lifecycle admin API (issue #225). No-op
	// without the tenancy service.
	s.registerTenantAdminRoutes(s.publicHandler)

	// Platform-superadmin cross-tenant admin API (issue #226), gated by
	// openrails:platform:superadmin in the SEPARATE platform org. No-op without
	// the control plane / platform org configured.
	s.registerPlatformRoutes(s.publicHandler)

	log.Info("Billing service initialized successfully")
	return s, nil
}

func (s *Server) newPublicEngine() *gin.Engine {
	e := gin.New()
	e.Use(gin.Recovery())
	e.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		SkipPaths: []string{"/health/live", "/health/ready", "/healthz", "/readyz", "/health"},
	}))
	e.Use(middleware.SecurityHeaders())
	// Allow-list = global CorsOrigins UNION every tenant's browser-direct
	// allowed origins (issue #222 browser tier). Preflight from a configured
	// tenant origin succeeds; unlisted origins are denied (never a wildcard
	// outside development).
	e.Use(middleware.CORS(s.cfg.AllowedCORSOrigins()))
	e.Use(middleware.BodyLimit(middleware.DefaultMaxBodyBytes))
	// Resolve the tenant / billing namespace before authorization and before any
	// tenant-owned DB access (issue #223). Defaults to the single default tenant.
	e.Use(middleware.ResolveTenant())
	if s.authProvider != nil {
		e.Use(s.authProvider.Optional())
	}
	e.Use(middleware.RateLimitWithChallengeStore(s.cfg.RateLimits, s.cfg.Captcha, s.rdb, s.captchaStore))
	return e
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

// Handler returns the full public HTTP surface: health + debug (dev only) + user
// + admin + webhooks + OAT-authenticated server-to-server service routes
// (issue #222). There is no separate private/service handler — embedded hosts use
// the in-process pkg/service facade (Embedded.Service()) or this same public
// surface. It is designed to be mounted at a path prefix via http.StripPrefix.
func (s *Server) Handler() http.Handler { return s.publicHandler }

// Close currently does not own underlying resources; callers should close the App.
func (s *Server) Close(_ context.Context) error {
	log.Info("Billing HTTP server shut down")
	return nil
}

func (s *Server) Cfg() *config.Config {
	return s.cfg
}
