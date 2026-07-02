package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/merchant"
)

type Dependencies struct {
	Config  *config.Config
	Cache   cache.Cache
	Runtime *app.Runtime
	Redis   *redis.Client
	// Authenticator is the framework-neutral auth boundary; billingauth.Optional
	// wraps it as the best-effort global middleware (#282/#670).
	Authenticator billingauth.Authenticator
	// ControlPlane is OpenRails' OpenRails-owned AuthKit control plane (#224).
	// REQUIRED (#469): the standalone surface always runs with a control
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
	cfg     *config.Config
	cache   cache.Cache
	runtime *app.Runtime
	rdb     *redis.Client
	// authenticator is the framework-neutral auth boundary (issue #282/#670 —
	// there is no gin auth provider any more; every surface uses this directly).
	authenticator billingauth.Authenticator
	// delegatedAuthenticator is the optional host-supplied identity seam for
	// the self-service surface (#339); see Dependencies.DelegatedAuthenticator.
	delegatedAuthenticator billingauth.DelegatedAuthenticator
	controlPlane           *controlplane.ControlPlane
	delegatedResolver      middleware.DelegatedResolver
	captchaStore           *captcha.ChallengeStore

	// merchants is the merchant provisioning + lifecycle + per-merchant secret service
	// (issue #225). It reuses the control plane's pgx pool (the openrails.*
	// control-plane DB) and permission-group provisioner, and is always built
	// (#469: the control plane is mandatory on this surface).
	merchants *merchants.Service

	// configuredMerchant scopes legacy single-merchant installs. Zero in standalone,
	// where the merchant is resolved per-credential.
	configuredMerchant merchant.ID

	// browserCORSOriginSource is the standalone browser CORS origin source.
	// Production leaves this nil and reads AuthKit remote_application state via
	// controlPlane; tests can override it without a live database.
	browserCORSOriginSource middleware.CORSOriginSource

	// publicHandler is the single "full surface" HTTP handler: health + user +
	// self/customer + merchant + control-plane auth + webhook routes AND the
	// API-key-authenticated server-to-server service routes (issue #222). It is
	// the framework-neutral net/http mux wrapped in the neutral middleware chain
	// (#670 — the standalone gin stack is gone; one HTTP stack everywhere).
	publicHandler http.Handler

	// routeTable records every registered route pattern (ServeMux syntax), the
	// standalone surface's introspectable route table for the #670 parity test.
	routeTable []string
}

// recordRoute appends a registered pattern to the server's route table.
func (s *Server) recordRoute(pattern string) {
	s.routeTable = append(s.routeTable, pattern)
}

// RouteTable returns every route pattern the standalone surface registered
// ("GET /v1/me/balance", ServeMux syntax). Used by the route-surface test.
func (s *Server) RouteTable() []string {
	out := make([]string, len(s.routeTable))
	copy(out, s.routeTable)
	return out
}

// handle registers a plain net/http handler on the mux and records it.
func (s *Server) handle(mux *http.ServeMux, pattern string, h http.Handler) {
	s.recordRoute(pattern)
	mux.Handle(pattern, h)
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
		cfg:                    deps.Config,
		cache:                  deps.Cache,
		runtime:                deps.Runtime,
		rdb:                    deps.Redis,
		authenticator:          deps.Authenticator,
		delegatedAuthenticator: deps.DelegatedAuthenticator,
		configuredMerchant:     deps.ConfiguredMerchant,
		controlPlane:           deps.ControlPlane,
		captchaStore:           captcha.NewChallengeStore(deps.Redis),
	}

	// Build the merchant provisioning/lifecycle/secret service (issue #225). It
	// reuses the control plane's pgx pool (the OpenRails-owned openrails.*
	// control-plane DB) and permission-group provisioner. The DB-backed secret
	// store is the self-hosted default and needs no live Vault; a managed
	// deployment swaps in the Vault-backed store with the same addressing.
	{
		secretBackend, err := merchantsecrets.Build(context.Background(), deps.Config, deps.ControlPlane.Pool())
		if err != nil {
			return nil, err
		}
		secretStore := secretBackend.Secrets
		solanaTransit := secretBackend.SolanaTransit

		tsvc, terr := merchants.NewService(deps.ControlPlane.Pool(), secretStore, config.ExpectedProviderEnvironment(s.cfg.IsTestMode()))
		if terr != nil {
			return nil, fmt.Errorf("build merchants service: %w", terr)
		}
		s.merchants = tsvc
		if deps.Runtime != nil {
			deps.Runtime.Merchants = tsvc
			// #661: gate the provider route surface on what OpenRails can actually do.
			deps.Runtime.RouteCapabilities = &routesurface.RuntimeCapabilities{
				SolanaCanSign: secretBackend.SolanaCanSign,
				SecretWrite:   secretBackend.SecretWrite,
			}
			if !secretBackend.SolanaCanSign && deps.Runtime.Rails.GetSolanaRail() != nil {
				log.Warn("solana: a Solana rail is configured but OpenRails cannot sign (no Vault connection and no local key); Solana signing routes are disabled — configure vault_transit or a local keypair")
			}
			if deps.Runtime.CheckoutService != nil {
				deps.Runtime.CheckoutService.SetMerchantSecretStore(secretStore)
				deps.Runtime.CheckoutService.SetRailMerchantAccountSecretResolver(tsvc)
			}
			if deps.Runtime.VaultService != nil {
				deps.Runtime.VaultService.SetMerchantSecretStore(secretStore)
				deps.Runtime.VaultService.SetRailMerchantAccountSecretResolver(tsvc)
			}
		}

		// Recurring Solana services (#254/#255/#256): build one per-merchant
		// Submitter (Transit when configured so the key never leaves Vault, else a
		// keypair signer over the secret store) and share it across the cranker,
		// plan-publish, and enroll services. The cranker MUST be injected BEFORE
		// workers start (InitRiver).
		if deps.Runtime != nil && deps.Runtime.SolanaRPC != nil {
			// solanaSigner is the SAME per-merchant signer the Submitter wraps. The
			// tier-change prepare service (#272) co-signs the merchant/cranker slot
			// with it directly, so it MUST be the same key as the cranker.
			solanaSigner := recurring.NewSignerFromRailMerchantAccounts(secretStore, solanaTransit, deps.Runtime.DB, 0, config.ExpectedProviderEnvironment(s.cfg.IsTestMode()))
			submitter := recurring.NewSignerSubmitter(solanaSigner, deps.Runtime.SolanaRPC)
			network := "mainnet"
			if pc := deps.Runtime.Rails.GetSolanaRail(); pc != nil && pc.Solana != nil && pc.Solana.Network != "" {
				network = pc.Solana.Network
			}
			var solanaTokens map[string]config.TokenConfig
			if pc := deps.Runtime.Rails.GetSolanaRail(); pc != nil && pc.Solana != nil {
				solanaTokens = pc.Solana.Tokens
			}
			cranker := recurring.NewCrankService(submitter)
			deps.Runtime.SetSolanaCranker(cranker)
			// Plan-publish (#254) + enroll-confirm (#255) HTTP surfaces. Enroll needs
			// the lifecycle (membership) + the on-chain subscription store.
			if deps.Runtime.SubscriptionLifecycleService != nil && deps.Runtime.DB != nil {
				planSvc := recurring.NewPlanServiceWithReader(submitter, deps.Runtime.SolanaRPC, network, solanaTokens)
				enrollSvc := recurring.NewEnrollService(
					deps.Runtime.SubscriptionLifecycleService,
					solanasubs.NewSolanaSubscriptionRepo(deps.Runtime.DB),
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
					solanasubs.NewSolanaSubscriptionRepo(deps.Runtime.DB),
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
					solanaSubRepo := solanasubs.NewSolanaSubscriptionRepo(deps.Runtime.DB)
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

	// Single (standalone-friendly) HTTP surface on the framework-neutral
	// net/http mux (#670): the same stack the embedded surface serves.
	mux := http.NewServeMux()
	// Standalone mode owns service-level health/meta routes.
	s.registerStandaloneMetaRoutes(mux)
	// Canonical: /v1/*
	s.registerUserRoutes(mux)
	// #555/#561: merchant/support routes live only under `/v1/merchant/*`.
	s.registerMerchantActionRoutes(mux)

	// Selective AuthKit route mounting (#224). In locked-down mode this mounts
	// ONLY the intentional AuthKit route groups (login/session/user) under
	// /auth — never AuthKit DefaultAPI.
	s.registerControlPlaneAuthRoutes(mux)

	// Browser-direct self-service API: delegated-access-token-authenticated, on
	// the SAME public surface (issue #222 browser tier). Always mounted (#469);
	// a host-supplied DelegatedAuthenticator overrides the control plane's
	// delegated-token verifier (#339).
	s.registerSelfServiceRoutes(mux)

	// Canonical provider-only webhook surface (#650): /v1/webhooks/:provider for NMI/CCBill
	// (their payloads carry account identity) and /v1/webhooks/:provider/:account_id for
	// direct Stripe. The handler resolves the provider account from the payload/route, derives
	// the owning merchant from that globally-unique account row, and verifies the signature
	// with THAT account's secret. This is the canonical multi-merchant shape.
	s.registerWebhookRoutes(mux)
	// Merchant-scoped webhook routing (issue #529): /v1/merchants/:merchant/webhooks/:provider
	// resolves the merchant from the path slug, then loads THAT merchant's signing
	// secret and verifies the signature AFTER merchant resolution. Kept as a transition alias
	// alongside the canonical provider-only surface above (#650).
	s.registerMerchantWebhookRoutes(mux)

	s.publicHandler = s.wrapPublicHandler(mux)

	log.Info("Billing service initialized successfully")
	return s, nil
}

// wrapPublicHandler applies the global middleware chain — the neutral analogue
// (and successor, #670) of the old gin engine's global middleware, same order.
func (s *Server) wrapPublicHandler(mux *http.ServeMux) http.Handler {
	return middleware.ChainHTTP(mux,
		middleware.RecoverHTTP(),
		middleware.RequestLogHTTP("/health/live", "/health/ready", "/healthz", "/readyz", "/health"),
		middleware.SecurityHeadersHTTP(),
		// CORS is browser transport policy, not API authorization. Preflight has no
		// JWT issuer. JWT signature/issuer/audience/permissions and merchant ownership
		// remain the real request security boundary; hosts that need browser CORS wire
		// an explicit origin source.
		middleware.CORSFromSourceHTTP(s.browserCORSOrigins),
		middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes),
		// Resolve the merchant / billing namespace before authorization and before any
		// merchant-owned DB access (issue #223). Pins the construction-time configured
		// merchant resolved once at boot (#336); zero when none is configured, in
		// which case merchant-owned operations hard-fail (there is no default merchant).
		middleware.ResolveMerchantHTTP(s.configuredMerchant),
		// Best-effort auth so the rate limiter can key by user, not only IP.
		middleware.HTTPMiddleware(billingauth.Optional(s.authenticator)),
		middleware.RateLimitHTTP(s.cfg.RateLimits, s.cfg.Captcha, s.rdb, s.captchaStore),
	)
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

// Handler returns the full public HTTP surface: health + user + self/customer
// + merchant + webhooks + API-key-authenticated server-to-server service routes
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
