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
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/middleware"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/merchant"
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
	// the browser-direct self-service and merchant surfaces (issue #339). When
	// set, the host verifies the incoming credential itself and supplies the
	// explicitly mapped principal for /v1/me/* + /v1/merchant/*, OVERRIDING the
	// control plane's default delegated-token verifier.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
	ConfiguredMerchant     merchant.ID
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
	delegatedResolver      ginmw.DelegatedResolver
	captchaStore           *captcha.ChallengeStore

	// merchants is the merchant provisioning + lifecycle + per-merchant secret service
	// (issue #225). It reuses the control plane's pgx pool (the openrails.*
	// control-plane DB) and operator-org provisioner, and is always built
	// (#469: the control plane is mandatory on this surface).
	merchants *merchants.Service

	// configuredMerchant scopes THIS engine instance to one merchant — set by embedded
	// hosts (embed.Options.Merchant) that run one engine per merchant. Zero in standalone,
	// where the merchant is resolved per-credential. OpenRails is multi-merchant either way.
	configuredMerchant merchant.ID

	// browserCORSOriginSource is the standalone browser CORS origin source.
	// Production leaves this nil and reads AuthKit remote_application state via
	// controlPlane; tests can override it without a live database.
	browserCORSOriginSource ginmw.CORSOriginSource

	// publicHandler is the single "full surface" HTTP handler. It includes
	// health + debug (dev only) + user + admin + webhook routes AND the
	// API-key-authenticated server-to-server service routes (issue #222). There is no
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
	if deps.Runtime.RailCustomerService == nil {
		return nil, fmt.Errorf("server runtime rail customer service is required")
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
		configuredMerchant:     deps.ConfiguredMerchant,
		controlPlane:           deps.ControlPlane,
		captchaStore:           captcha.NewChallengeStore(deps.Redis),
	}

	// Build the merchant provisioning/lifecycle/secret service (issue #225). It
	// reuses the control plane's pgx pool (the OpenRails-owned openrails.*
	// control-plane DB) and operator-org provisioner. The DB-backed secret
	// store is the self-hosted default and needs no live Vault; a managed
	// deployment swaps in the Vault-backed store with the same addressing.
	{
		secretBackend, err := merchantsecrets.Build(context.Background(), deps.Config, deps.ControlPlane.Pool())
		if err != nil {
			return nil, err
		}
		secretStore := secretBackend.Secrets
		solanaTransit := secretBackend.SolanaTransit

		tsvc, terr := merchants.NewService(deps.ControlPlane.Pool(), secretStore)
		if terr != nil {
			return nil, fmt.Errorf("build merchants service: %w", terr)
		}
		s.merchants = tsvc
		if deps.Runtime != nil {
			deps.Runtime.Merchants = tsvc
			if deps.Runtime.CheckoutService != nil {
				deps.Runtime.CheckoutService.SetMerchantSecretStore(secretStore)
				deps.Runtime.CheckoutService.SetProviderAccountSecretResolver(tsvc)
			}
			if deps.Runtime.VaultService != nil {
				deps.Runtime.VaultService.SetMerchantSecretStore(secretStore)
				deps.Runtime.VaultService.SetProviderAccountSecretResolver(tsvc)
			}
		}

		// Single-install bridge (#253): an in-memory Solana private key is seeded
		// only into the CONFIGURED merchant's secret store as solana/private_key
		// (#336: no default merchant — the seed no-ops when no merchant is configured).
		// Named tenants must be configured explicitly by an operator credential
		// rotation. Idempotent; never overwrites an existing secret. No-op when
		// Solana is unconfigured or uses Vault Transit (non-extractable key, so no
		// global private key is set).
		if pc := deps.Runtime.Rails.GetSolanaRail(); pc != nil {
			if err := recurring.SeedConfiguredMerchantSolanaSecret(context.Background(), secretStore, s.configuredMerchant, pc.PrivateKey); err != nil {
				return nil, fmt.Errorf("seed configured merchant solana secret: %w", err)
			}
		}

		// Recurring Solana services (#254/#255/#256): build one per-merchant
		// Submitter (Transit when configured so the key never leaves Vault, else a
		// keypair signer over the secret store) and share it across the cranker,
		// plan-publish, and enroll services. The cranker MUST be injected BEFORE
		// workers start (InitRiver).
		if deps.Runtime != nil && deps.Runtime.SolanaRPC != nil {
			var submitter recurring.Submitter
			// solanaSigner is the SAME per-merchant signer the Submitter wraps. The
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
			if pc := deps.Runtime.Rails.GetSolanaRail(); pc != nil && pc.Network != "" {
				network = pc.Network
			}
			var solanaTokens map[string]config.TokenConfig
			if pc := deps.Runtime.Rails.GetSolanaRail(); pc != nil {
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
				// checkout session service so /v1/me/checkout(+confirm) drives the
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

	}

	// Single (standalone-friendly) HTTP surface.
	// Standalone mode owns service-level health/debug routes.
	s.publicHandler = s.newPublicEngine()
	s.registerStandaloneMetaRoutes(s.publicHandler)
	// Canonical: /v1/*
	s.registerUserRoutes(s.publicHandler)
	// #555/#561: merchant/support routes live only under `/v1/merchant/*`.
	s.registerMerchantActionRoutesOn(s.publicHandler)

	// Selective AuthKit route mounting (#224). In locked-down mode this mounts
	// ONLY the intentional AuthKit route groups (login/session/user) under
	// /auth — never AuthKit DefaultAPI.
	s.registerControlPlaneAuthRoutes(s.publicHandler)

	// #555 HARD CUT: the server-to-server billing routes moved from the gin
	// `/v1/service/*` surface to the router-based `/v1/merchant/*` surface mounted
	// by registerMerchantActionRoutesOn above. The `/v1/service` duplicate is gone.

	// Browser-direct self-service API: delegated-access-token-authenticated, on
	// the SAME public engine (issue #222 browser tier). Always mounted (#469);
	// a host-supplied DelegatedAuthenticator overrides the control plane's
	// delegated-token verifier (#339).
	s.registerSelfServiceRoutes(s.publicHandler)

	// Merchant-scoped webhook routing (issue #529): /v1/merchants/:merchant/webhooks/:provider
	// resolves the merchant from the path slug, then loads THAT merchant's signing
	// secret and verifies the signature AFTER merchant resolution.
	s.registerMerchantWebhookRoutes(s.publicHandler)

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
	// CORS is browser transport policy, not API authorization. Preflight has no
	// JWT issuer, so standalone uses the union of enabled AuthKit
	// remote_application.allowed_origins and fails closed when the registry cannot
	// be read. JWT signature/issuer/audience/permissions and merchant ownership
	// remain the real request security boundary.
	e.Use(ginmw.CORSFromSource(s.browserCORSOrigins))
	e.Use(ginmw.BodyLimit(middleware.DefaultMaxBodyBytes))
	// Resolve the merchant / billing namespace before authorization and before any
	// merchant-owned DB access (issue #223). Pins the construction-time configured
	// merchant resolved once at boot (#336); zero when none is configured, in
	// which case merchant-owned operations hard-fail (there is no default merchant).
	e.Use(ginmw.ResolveMerchant(s.configuredMerchant))
	if s.authProvider != nil {
		e.Use(s.authProvider.Optional())
	}
	e.Use(ginmw.RateLimitWithChallengeStore(s.cfg.RateLimits, s.cfg.Captcha, s.rdb, s.captchaStore))
	return e
}

func (s *Server) browserCORSOrigins(ctx context.Context) ([]string, error) {
	if s != nil && s.browserCORSOriginSource != nil {
		return s.browserCORSOriginSource(ctx)
	}
	if s == nil || s.controlPlane == nil {
		return nil, controlplane.ErrDelegatedNotConfigured
	}
	return s.controlPlane.BrowserCORSOrigins(ctx)
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
		RDB:           s.rdb,
		Authenticator: s.embeddedAuthenticator(),
	}
	// The control plane is always present on this surface (#469); it is the
	// live admin-permission checker.
	asm.AdminChecker = s.controlPlane
	asm.ServiceCredentialResolver = s.controlPlane
	return asm.NewHTTPHandler(embedhttp.Options{
		RouteSets: opts.RouteSets,
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
// + admin + webhooks + API-key-authenticated server-to-server service routes
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
