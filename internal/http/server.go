package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/crypto"
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/middleware"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/platform"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
)

type Dependencies struct {
	Config  *config.Config
	Cache   cache.Cache
	Runtime *app.Runtime
	Redis   *redis.Client
	// Authenticator is the framework-neutral auth boundary (gin-free). The gin
	// Optional()/Required() middleware used by the standalone surface is
	// reconstructed from it via ginauth.ProviderFromAuthenticator (#285).
	Authenticator billingauth.Authenticator
	// ControlPlane is OpenRails' OpenRails-owned AuthKit control plane (#224).
	// REQUIRED (#469): the standalone gin surface always runs with a control
	// plane — there is no verifier-only mode. The server selectively mounts the
	// intentional AuthKit route groups (never DefaultAPI in locked-down mode).
	ControlPlane *controlplane.ControlPlane
	// DelegatedAuthenticator is the OPTIONAL host-pluggable identity seam for
	// the browser-direct self-service surface (issue #339). When set, the host
	// verifies the incoming credential itself and supplies the explicitly
	// mapped principal for /v1/self/* + /v1/tenant-admin/*, OVERRIDING the
	// control plane's default delegated-token verifier.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
}

type Server struct {
	cfg          *config.Config
	cache        cache.Cache
	runtime      *app.Runtime
	rdb          *redis.Client
	authProvider ginauth.Provider
	// authenticator is the framework-neutral auth boundary used by the gin-free
	// embedded surface (issue #282). It is derived from authProvider when that
	// provider exposes one (AuthKit-backed + ProviderFromAuthenticator do); a host
	// may also set it explicitly. nil when neither is available — embedded
	// auth-gated routes then fail closed with 500 (see Options.requiredMW).
	authenticator billingauth.Authenticator
	// delegatedAuthenticator is the optional host-supplied identity seam for
	// the self-service surface (#339); see Dependencies.DelegatedAuthenticator.
	delegatedAuthenticator billingauth.DelegatedAuthenticator
	controlPlane           *controlplane.ControlPlane
	captchaStore           *captcha.ChallengeStore

	// tenancy is the tenant provisioning + lifecycle + per-tenant secret service
	// (issue #225). It reuses the control plane's pgx pool (the openrails.*
	// control-plane DB) and operator-tenant provisioner, and is always built
	// (#469: the control plane is mandatory on this surface).
	tenancy *tenancy.Service

	// Platform superadmin layer (issue #226), DISTINCT from per-tenant operator
	// admin, sharing the openrails.* control-plane pool. platformAudit records
	// every cross-tenant superadmin mutation; platformBreakGlass manages
	// time-boxed elevation; platformMetrics aggregates platform-wide tenant
	// metrics. The /v1/platform/* surface itself is mounted only when a
	// platform tenant is configured.
	platformAudit      *platform.AuditLog
	platformBreakGlass *platform.BreakGlass
	platformMetrics    *platform.Metrics

	// publicHandler is the single "full surface" HTTP handler. It includes
	// health + debug (dev only) + user + admin + webhook routes AND the
	// service token-authenticated server-to-server service routes (issue #222). There is no
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
	if deps.Authenticator == nil {
		return nil, fmt.Errorf("authenticator is required")
	}
	// HARD CUT (#469): the standalone surface always runs with the AuthKit
	// control plane; a missing control plane is a boot failure, not a degraded
	// "verifier-only" server.
	if deps.ControlPlane == nil {
		return nil, fmt.Errorf("control plane is required (#469: standalone always runs the AuthKit control plane)")
	}
	if deps.ControlPlane.Pool() == nil {
		return nil, fmt.Errorf("control plane pool is required")
	}

	s := &Server{
		cfg:     deps.Config,
		cache:   deps.Cache,
		runtime: deps.Runtime,
		rdb:     deps.Redis,
		// The gin standalone surface needs Optional()/Required() middleware: build
		// the gin provider from the framework-neutral authenticator (#285). The
		// embedded net/http surface uses the authenticator directly.
		authProvider:           ginauth.ProviderFromAuthenticator(deps.Authenticator),
		authenticator:          deps.Authenticator,
		delegatedAuthenticator: deps.DelegatedAuthenticator,
		controlPlane:           deps.ControlPlane,
		captchaStore:           captcha.NewChallengeStore(deps.Redis),
	}

	// Wire the live admin-permission checker onto the runtime so mixed
	// public/admin read endpoints (e.g. catalog inactive rows) can evaluate
	// openrails:admin for the caller's own tenant (#312).
	deps.Runtime.AdminChecker = deps.ControlPlane

	// Build the tenant provisioning/lifecycle/secret service (issue #225). It
	// reuses the control plane's pgx pool (the OpenRails-owned openrails.*
	// control-plane DB) and operator-tenant provisioner. The DB-backed secret
	// store is the self-hosted default and needs no live Vault; a managed
	// deployment swaps in the Vault-backed store with the same addressing.
	{
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
				Address: vc.Address, AuthMethod: vc.AuthMethod, Token: vc.Token, RoleID: vc.RoleID,
				SecretID: vc.SecretID, K8sRole: vc.K8sRole, KVMount: kvMount, TransitMount: vc.TransitMount,
			})
			if verr != nil {
				return nil, fmt.Errorf("vault login: %w", verr)
			}
			secretStore = tenancy.NewVaultSecretStore(kvMount, vault.NewKVv2Adapter(vclient, kvMount), tenancy.NewDBTenantSlugResolver(deps.ControlPlane.Pool()))
			if vc.UseTransitForSolana {
				tMount := vc.TransitMount
				if tMount == "" {
					tMount = "transit"
				}
				solanaTransit = vault.NewTransitAdapter(vclient, tMount)
			}
		} else {
			// Self-hosted default: DB-backed store + per-tenant envelope encryption
			// (issue #227). With no master key, most secrets keep the legacy
			// plaintext behavior, but Solana private keys are write-blocked below so
			// recurring signing keys are never newly stored plaintext.
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
			if !enc.Enabled() {
				secretStore = tenancy.NewWriteRestrictedSecretStore(secretStore, map[string]string{
					solanaint.SecretSolanaPrivateKey: "ENCRYPTION_MASTER_KEY is required before storing DB-backed Solana private keys",
				})
			}
		}

		// Front the chosen store with an in-process TTL cache (#251/#322) so workers
		// and hot paths resolve a tenant's secret once per window instead of per row /
		// request. Default ~15m; a rotation through this node invalidates immediately.
		// Negative TTL disables it (NewCachedSecretStore returns the store unchanged).
		secretCacheTTL := tenancy.DefaultSecretCacheTTL
		if deps.Config != nil && deps.Config.Vault != nil && deps.Config.Vault.SecretCacheTTLSeconds != 0 {
			secretCacheTTL = time.Duration(deps.Config.Vault.SecretCacheTTLSeconds) * time.Second
		}
		secretStore = tenancy.NewCachedSecretStore(secretStore, secretCacheTTL)

		tsvc, terr := tenancy.NewService(deps.ControlPlane.Pool(), deps.ControlPlane, secretStore)
		if terr != nil {
			return nil, fmt.Errorf("build tenancy service: %w", terr)
		}
		s.tenancy = tsvc
		if deps.Runtime != nil {
			deps.Runtime.Tenancy = tsvc
			if deps.Runtime.CheckoutService != nil {
				deps.Runtime.CheckoutService.SetTenantSecretStore(secretStore)
			}
			if deps.Runtime.VaultService != nil {
				deps.Runtime.VaultService.SetTenantSecretStore(secretStore)
			}
		}

		// Single-install bridge (#253): a global-config Solana private key is seeded
		// only into the default tenant's secret store as solana/private_key. Named
		// tenants must be configured explicitly by an operator credential rotation.
		// Idempotent; never overwrites an existing secret. No-op when Solana is
		// unconfigured or uses Vault Transit (non-extractable key, so no global
		// private key is set).
		if pc := deps.Config.GetSolanaProcessor(); pc != nil {
			if err := recurring.SeedDefaultTenantSolanaSecret(context.Background(), secretStore, pc.PrivateKey); err != nil {
				return nil, fmt.Errorf("seed default tenant solana secret: %w", err)
			}
		}

		// Recurring Solana services (#254/#255/#256): build one per-tenant
		// Submitter (Transit when configured so the key never leaves Vault, else a
		// keypair signer over the secret store) and share it across the cranker,
		// plan-publish, and enroll services. The cranker MUST be injected BEFORE
		// workers start (InitRiver).
		if deps.Runtime != nil && deps.Runtime.SolanaRPC != nil {
			var submitter recurring.Submitter
			// solanaSigner is the SAME per-tenant signer the Submitter wraps. The
			// tier-change prepare service (#272) co-signs the merchant/cranker slot
			// with it directly, so it MUST be the same key as the cranker.
			var solanaSigner solanaint.Signer
			if solanaTransit != nil {
				solanaSigner = recurring.NewSignerFromTransit(solanaTransit, 0)
				submitter = recurring.NewSignerSubmitterFromTransit(solanaTransit, deps.Runtime.SolanaRPC, 0)
			} else {
				solanaSigner = recurring.NewSignerFromStore(secretStore, 0)
				submitter = recurring.NewSignerSubmitterFromStore(secretStore, deps.Runtime.SolanaRPC, 0)
			}
			network := "mainnet"
			if pc := deps.Config.GetSolanaProcessor(); pc != nil && pc.Network != "" {
				network = pc.Network
			}
			var solanaTokens map[string]config.SolanaToken
			if pc := deps.Config.GetSolanaProcessor(); pc != nil {
				solanaTokens = pc.Tokens
			}
			cranker := recurring.NewCrankService(submitter)
			deps.Runtime.SetSolanaCranker(cranker)
			// Plan-publish (#254) + enroll-confirm (#255) HTTP surfaces. Enroll needs
			// the lifecycle (membership) + the on-chain subscription store.
			if deps.Runtime.SubscriptionLifecycleService != nil && deps.Runtime.DB != nil {
				planSvc := recurring.NewPlanServiceWithReader(submitter, deps.Runtime.SolanaRPC, network, solanaTokens)
				enrollSvc := recurring.NewEnrollService(
					deps.Runtime.SubscriptionLifecycleService,
					dbrepo.NewSolanaSubscriptionRepo(deps.Runtime.DB),
					deps.Runtime.SolanaRPC,
					submitter,
					network,
					solanaTokens,
				)
				deps.Runtime.SetSolanaRecurringServices(planSvc, enrollSvc)

				// App-driven on-chain cancel (#266): builds the unsigned
				// cancel_subscription tx the subscriber signs to trustlessly revoke a
				// recurring subscription on-chain (additive to the soft cancel #264).
				deps.Runtime.SetSolanaPrepareCancelService(recurring.NewPrepareCancelService(
					dbrepo.NewSolanaSubscriptionRepo(deps.Runtime.DB),
					deps.Runtime.SolanaRPC,
				))

				// App-driven on-chain tier change (#272): the prepare/confirm pair.
				// PrepareTierChangeService builds the SINGLE ATOMIC tx (cancel-old +
				// subscribe-new [+ prorated transfer for an upgrade]); for an upgrade it
				// co-signs the merchant/cranker slot with the SAME signer + RPC + network
				// as the cranker, so the slot it pre-signs is the merchant's own key.
				deps.Runtime.SetSolanaPrepareTierChangeService(recurring.NewPrepareTierChangeService(
					solanaSigner,
					deps.Runtime.SolanaRPC,
					network,
					solanaTokens,
				))

				// Subscribe-via-checkout (#261/#262): the prepare service builds the
				// unsigned init/subscribe txns; enroll confirms. Wire both into the
				// checkout session service so /v1/self/checkout(+confirm) drives the
				// recurring Solana subscription flow.
				if deps.Runtime.CheckoutSessionService != nil {
					// The subscribe step is now an ATOMIC co-signed bundle (#286):
					// [subscribe + transfer(first period)]. The cranker pre-signs the
					// transfer slot, so the prepare service takes the SAME signer + RPC +
					// network as the cranker (the slot it pre-signs is the merchant's key).
					prepareSvc := recurring.NewPrepareSubscribeService(submitter, solanaSigner, deps.Runtime.SolanaRPC, network, solanaTokens)
					deps.Runtime.CheckoutSessionService.SetSolanaRecurring(prepareSvc, enrollSvc)

					// Cancel + tier-change as Solana Pay checkout modes: reuse the same
					// prepare services as the auth-gated #271/#272 handlers, and build the
					// confirm services (mirrors) for the reference poller to invoke on
					// confirmation. This extends the existing Solana Pay machinery to the
					// solana_cancel / solana_tier_change modes — no parallel protocol.
					solanaSubRepo := dbrepo.NewSolanaSubscriptionRepo(deps.Runtime.DB)
					confirmCancelSvc := recurring.NewConfirmCancelService(
						deps.Runtime.SolanaRPC,
						deps.Runtime.SubscriptionLifecycleService,
					)
					confirmTierChangeSvc := recurring.NewConfirmTierChangeService(
						deps.Runtime.SolanaRPC,
						deps.Runtime.SubscriptionLifecycleService,
						solanaSubRepo,
						network,
						solanaTokens,
					)
					deps.Runtime.CheckoutSessionService.SetSolanaLifecycle(
						deps.Runtime.SolanaPrepareCancelService,
						deps.Runtime.SolanaPrepareTierChangeService,
						confirmCancelSvc,
						confirmTierChangeSvc,
						deps.Runtime.SubscriptionService,
						solanaSubRepo,
					)
				}
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
	// /auth — never AuthKit DefaultAPI.
	s.registerControlPlaneAuthRoutes(s.publicHandler)

	// Server-to-server service API: service token-authenticated, on the SAME
	// public engine (issue #222). No private port, no mTLS listener.
	s.registerServiceRoutes(s.publicHandler)

	// Browser-direct self-service API: delegated-access-token-authenticated, on
	// the SAME public engine (issue #222 browser tier). Always mounted (#469);
	// a host-supplied DelegatedAuthenticator overrides the control plane's
	// delegated-token verifier (#339).
	s.registerSelfServiceRoutes(s.publicHandler)

	// Tenant-scoped webhook routing (issue #225): /v1/t/:tenant/webhooks/:provider
	// resolves the tenant from the path slug, then loads THAT tenant's signing
	// secret and verifies the signature AFTER tenant resolution.
	s.registerTenantWebhookRoutes(s.publicHandler)

	// Operator-gated tenant provisioning/lifecycle admin API (issue #225).
	s.registerTenantAdminRoutes(s.publicHandler)

	// Platform-superadmin cross-tenant admin API (issue #226), gated by
	// openrails:platform:superadmin in the SEPARATE platform tenant. No-op without
	// the control plane / platform tenant configured.
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
	e.Use(ginmw.SecurityHeaders())
	// Allow-list = global CorsOrigins UNION every tenant's browser-direct
	// allowed origins (issue #222 browser tier). Preflight from a configured
	// tenant origin succeeds; unlisted origins are denied (never a wildcard
	// outside development).
	e.Use(ginmw.CORS(s.cfg.AllowedCORSOrigins()))
	e.Use(ginmw.BodyLimit(middleware.DefaultMaxBodyBytes))
	// Resolve the tenant / billing namespace before authorization and before any
	// tenant-owned DB access (issue #223). Defaults to the single default tenant.
	e.Use(ginmw.ResolveTenant())
	if s.authProvider != nil {
		e.Use(s.authProvider.Optional())
	}
	e.Use(ginmw.RateLimitWithChallengeStore(s.cfg.RateLimits, s.cfg.Captcha, s.rdb, s.captchaStore))
	return e
}

// newHTTPHandlerMux delegates to the gin-free embedhttp assembler (issue
// #282/#285), which builds the embedded billing surface as a net/http handler
// with zero gin on the request path. The gin Server holds the standalone surface
// (newPublicEngine); embedded hosts use this gin-free handler via pkg/embedded.
func (s *Server) newHTTPHandlerMux(opts HTTPHandlerOptions) http.Handler {
	asm := &embedhttp.Assembler{
		Cfg:           s.cfg,
		Runtime:       s.runtime,
		CaptchaStore:  s.captchaStore,
		Authenticator: s.embeddedAuthenticator(),
	}
	// The control plane is always present on this surface (#469); it is the
	// live admin-permission checker.
	asm.AdminChecker = s.controlPlane
	return asm.NewHTTPHandler(embedhttp.Options{
		IncludeUser:     opts.IncludeUser,
		IncludeAdmin:    opts.IncludeAdmin,
		IncludeWebhooks: opts.IncludeWebhooks,
	})
}

// NewHTTPHandler returns a single mountable `http.Handler` for the selected route groups.
//
// Intended for embedded hosts.
//
// Embedded routes live under `/billing/v1/*`.
func (s *Server) NewHTTPHandler(opts HTTPHandlerOptions) http.Handler {
	return s.newHTTPHandlerMux(opts)
}

// embeddedAuthenticator returns the framework-neutral Authenticator used by the
// embedded route surface (issue #282), or nil when none is available.
func (s *Server) embeddedAuthenticator() billingauth.Authenticator {
	return s.authenticator
}

// Handler returns the full public HTTP surface: health + debug (dev only) + user
// + admin + webhooks + service token-authenticated server-to-server service routes
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
