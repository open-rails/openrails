package server

import (
	"context"
	"fmt"
	"io/fs"
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
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/shared/iputil"
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
	// ConsoleAssets is the built admin console SPA (#754: the engine ships no
	// frontend bytes; whoever builds the binary owns the embed). nil = absent.
	// /admin mounts only when this is present AND admin_console.enabled;
	// enabled without assets is a boot error.
	ConsoleAssets fs.FS
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
	// consoleAssets is the host/binary-supplied admin console build (#754).
	consoleAssets fs.FS

	// merchants is the merchant provisioning + lifecycle + per-merchant secret service
	// (issue #225). It reuses the control plane's pgx pool (the openrails.*
	// control-plane DB) and permission-group provisioner, and is always built
	// (#469: the control plane is mandatory on this surface).
	merchants *merchants.Service

	// browserTierRoutes tracks which registered patterns belong to the
	// permissive-CORS browser tier (#765: checkout + self-service +
	// customer-treasury) — populated as registerUserRoutesAt/
	// registerSelfServiceRoutes mount their routes, consulted by
	// wrapPublicHandler's PermissiveCORSHTTP. Never nil once New() has run.
	browserTierRoutes *middleware.BrowserTierRoutes

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

// recordBrowserRoute is recordRoute plus browser-tier CORS registration
// (#765): used ONLY by the checkout + self-service + customer-treasury
// registration call sites, so PermissiveCORSHTTP grants the static `*` policy
// on exactly those routes and nothing else. Lazily initializes
// browserTierRoutes so hand-built *Server{} unit tests that skip New() still
// behave correctly.
func (s *Server) recordBrowserRoute(pattern string) {
	s.recordRoute(pattern)
	if s.browserTierRoutes == nil {
		s.browserTierRoutes = middleware.NewBrowserTierRoutes()
	}
	s.browserTierRoutes.Add(pattern)
}

// RouteTable returns the registered route surface (guard tests).
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

// handleBrowser is handle plus browser-tier CORS registration (#765): use for
// raw net/http handlers (not routed through router.Router) that belong to the
// checkout/self-service surface, e.g. the captcha discovery endpoints
// registered alongside RegisterUserRoutes.
func (s *Server) handleBrowser(mux *http.ServeMux, pattern string, h http.Handler) {
	s.handle(mux, pattern, h)
	if s.browserTierRoutes == nil {
		s.browserTierRoutes = middleware.NewBrowserTierRoutes()
	}
	s.browserTierRoutes.Add(pattern)
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
	if deps.Runtime.RailPaymentMethodService == nil {
		return nil, fmt.Errorf("server runtime payment method service is required")
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
		controlPlane:           deps.ControlPlane,
		captchaStore:           captcha.NewChallengeStore(deps.Redis),
		consoleAssets:          deps.ConsoleAssets,
		browserTierRoutes:      middleware.NewBrowserTierRoutes(),
	}

	// Build the merchant provisioning/lifecycle/secret service (issue #225). It
	// reuses the control plane's pgx pool (the OpenRails-owned openrails.*
	// control-plane DB) and permission-group provisioner. MODE 1 (#723,
	// merchant_source=manifest) serves the runtime's in-memory manifest plane —
	// no DB/Vault store exists; the write routes stay mounted and 405 with the
	// manifest_driven code. MODE 2 builds the declared persistent backend.
	{
		var secretBackend *merchantsecrets.Store
		if deps.Config.IsManifestMerchantSource() {
			if deps.Runtime == nil || deps.Runtime.ManifestSecrets == nil {
				return nil, fmt.Errorf("merchant_source=manifest requires the runtime manifest secret plane (#723)")
			}
			b, err := merchantsecrets.BuildManifest(context.Background(), deps.Config, deps.Runtime.ManifestSecrets)
			if err != nil {
				return nil, err
			}
			secretBackend = b
		} else {
			b, err := merchantsecrets.Build(context.Background(), deps.Config, deps.ControlPlane.Pool())
			if err != nil {
				return nil, err
			}
			secretBackend = b
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
			// #748: live-reachability probe for /readyz (nil-safe no-op for the
			// manifest plane / DB-backed store — only a Vault-backed store checks).
			deps.Runtime.MerchantSecretPing = secretBackend.Ping
			// #661: gate the provider route surface on what OpenRails can actually do.
			deps.Runtime.RouteCapabilities = &routesurface.RuntimeCapabilities{
				SolanaCanSign: secretBackend.SolanaCanSign,
				SecretWrite:   secretBackend.SecretWrite,
			}
			if deps.Runtime.CheckoutService != nil {
				deps.Runtime.CheckoutService.SetMerchantSecretStore(secretStore)
				deps.Runtime.CheckoutService.SetRailMerchantAccountSecretResolver(tsvc)
			}
			if deps.Runtime.RailPaymentMethodService != nil {
				deps.Runtime.RailPaymentMethodService.SetMerchantSecretStore(secretStore)
				deps.Runtime.RailPaymentMethodService.SetRailMerchantAccountSecretResolver(tsvc)
			}
		}

		// Recurring Solana services (#254/#255/#256): build one per-merchant
		// Submitter (Transit when configured so the key never leaves Vault, else a
		// keypair signer over the secret store) and share it across the cranker,
		// plan-publish, and enroll services. The cranker MUST be injected BEFORE
		// workers start (InitRiver). The Submitter's RPC resolves PER MERCHANT at
		// submit time (#728: store settings win, boot client fallback), so the
		// cranker arms even for store-only merchants (no boot Solana rail).
		if deps.Runtime != nil {
			// solanaSigner is the SAME per-merchant signer the Submitter wraps. The
			// tier-change prepare service (#272) co-signs the merchant/cranker slot
			// with it directly, so it MUST be the same key as the cranker.
			solanaSigner := recurring.NewSignerFromRailMerchantAccounts(secretStore, solanaTransit, deps.Runtime.DB, 0, config.ExpectedProviderEnvironment(s.cfg.IsTestMode()))
			submitter := recurring.NewSignerSubmitterWithResolver(solanaSigner, deps.Runtime.SolanaRPCResolver.Resolve)
			// #788: network derives from test_mode alone (#349); the token set
			// is the network's curated default (a merchant's rail-account
			// settings refine tokens at request-time resolution seams).
			network := "mainnet"
			if s.cfg.IsTestMode() {
				network = "devnet"
			}
			solanaTokens := solanatokens.ForNetwork(network)
			cranker := recurring.NewCrankService(submitter)
			deps.Runtime.SetSolanaCranker(cranker)
			// Plan-publish (#254) + enroll-confirm (#255) HTTP surfaces. Chain
			// reads arm PER MERCHANT through the #728 resolver at request time.
			chainReader := deps.Runtime.SolanaRPCResolver.ChainReader()
			if deps.Runtime.SubscriptionLifecycleService != nil && deps.Runtime.DB != nil {
				planSvc := recurring.NewPlanServiceWithReader(submitter, chainReader, network, solanaTokens)
				enrollSvc := recurring.NewEnrollService(
					deps.Runtime.SubscriptionLifecycleService,
					solanasubs.NewSolanaSubscriptionRepo(deps.Runtime.DB),
					chainReader,
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
					chainReader,
				))

				// App-driven on-chain tier change (#272): the prepare/confirm pair.
				// PrepareTierChangeService builds the SINGLE ATOMIC tx (cancel-old +
				// subscribe-new [+ prorated transfer for an upgrade]); for an upgrade it
				// co-signs the merchant/cranker slot with the SAME signer + RPC + network
				// as the cranker, so the slot it pre-signs is the merchant's own key.
				deps.Runtime.SetSolanaPrepareTierChangeService(recurring.NewPrepareTierChangeService(
					solanaSigner,
					chainReader,
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
					prepareSvc := recurring.NewPrepareSubscribeService(submitter, solanaSigner, chainReader, network, solanaTokens)
					deps.Runtime.CheckoutSessionService.SetSolanaRecurring(prepareSvc, enrollSvc)

					// Cancel + tier-change as Solana Pay checkout modes: reuse the same
					// prepare services as the auth-gated #271/#272 handlers, and build the
					// confirm services (mirrors) for the reference poller to invoke on
					// confirmation. This extends the existing Solana Pay machinery to the
					// solana_cancel / solana_tier_change modes — no parallel protocol.
					solanaSubRepo := solanasubs.NewSolanaSubscriptionRepo(deps.Runtime.DB)
					confirmCancelSvc := recurring.NewConfirmCancelService(
						chainReader,
						deps.Runtime.SubscriptionLifecycleService,
					)
					confirmTierChangeSvc := recurring.NewConfirmTierChangeService(
						chainReader,
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
	// #721: cross-merchant platform operator directory (/v1/platform/*),
	// standalone-only (root-group-gated; no embedded analogue).
	s.registerPlatformRoutes(mux)

	// Selective AuthKit route mounting (#224, authhttp.MountHandler since
	// authkit #250). In locked-down mode this mounts ONLY the intentional
	// AuthKit route groups (login/session/user) under /auth — never AuthKit
	// DefaultAPI.
	if err := s.registerControlPlaneAuthRoutes(mux); err != nil {
		return nil, err
	}

	// Merchant admin console SPA (#740/#754): mounted only when assets are
	// present AND admin_console.enabled; enabled without assets refuses boot.
	if err := s.registerAdminConsoleRoutes(mux); err != nil {
		return nil, err
	}

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

// trustedProxies returns the #746 client-IP resolver, nil-safe against a
// Server built without New() (some unit tests construct &Server{} directly).
func (s *Server) trustedProxies() *iputil.TrustedProxies {
	if s == nil || s.runtime == nil {
		return nil
	}
	return s.runtime.TrustedProxies
}

// httpIdempotencyService returns the runtime's client-facing Idempotency-Key
// replay store (#579), nil-safe against a Server built without New() (some
// unit tests construct &Server{} directly and call wrapPublicHandler, e.g.
// routes_self_test.go).
func (s *Server) httpIdempotencyService() *idempotency.IdempotencyService {
	if s == nil || s.runtime == nil {
		return nil
	}
	return s.runtime.HTTPIdempotency
}

// wrapPublicHandler applies the global middleware chain — the neutral analogue
// (and successor, #670) of the old gin engine's global middleware, same order.
func (s *Server) wrapPublicHandler(mux *http.ServeMux) http.Handler {
	return middleware.ChainHTTP(mux,
		middleware.RecoverHTTP(),
		middleware.RequestLogHTTP("/health/live", "/health/ready", "/healthz", "/readyz", "/health"),
		middleware.SecurityHeadersHTTP(),
		// CORS is browser transport policy, not API authorization; real request
		// security is always JWT signature/issuer/audience/permissions plus
		// merchant ownership, which a browser preflight can't even carry (no
		// JWT on OPTIONS). #765: since every accepted request is a bearer JWT
		// (never an ambient cookie), an origin allow-list protects nothing —
		// a stolen token is replayed from curl, where CORS doesn't exist — so
		// the policy is a static, non-configurable `*` grant on exactly the
		// browser-tier routes (checkout + self-service + customer-treasury,
		// tracked in browserTierRoutes as they register) and NO CORS headers
		// anywhere else (admin/platform/merchant-API/webhooks/auth), so a
		// browser refuses cross-origin script access to those by default.
		middleware.PermissiveCORSHTTP(s.browserTierRoutes.Match),
		middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes),
		// Resolve the merchant / billing namespace before authorization and before any
		// merchant-owned DB access (issue #223). Resolved PER REQUEST off the Runtime
		// (#744), never a value snapshotted here at construction time; zero when none
		// is configured, in which case merchant-owned operations hard-fail (there is
		// no default merchant).
		middleware.ResolveMerchantHTTP(s.runtime.ConfiguredMerchant),
		// #734: Host-based multi-merchant resolution, sharing the SAME control-plane
		// directory lookup as CORS above and as the JWT-issuer resolution
		// (internal/controlplane.merchantForIssuer checks the marker this pins).
		// Runs after the static ResolveMerchantHTTP so a live per-request Host match
		// is authoritative over any process-wide configured merchant. A Host with no
		// api_host configured for any merchant is a no-op — behavior is unchanged.
		middleware.ResolveMerchantFromHostHTTP(s.hostMerchantResolver),
		// Best-effort auth so the rate limiter can key by user, not only IP.
		middleware.HTTPMiddleware(billingauth.Optional(s.authenticator)),
		middleware.RateLimitHTTP(s.cfg.RateLimits, s.cfg.Captcha, s.rdb, s.captchaStore, s.trustedProxies()),
		// #579: client-facing Idempotency-Key replay (opt-in per request).
		middleware.IdempotencyHTTP(s.httpIdempotencyService()),
	)
}

// hostMerchantResolver is the standalone #734 Host->merchant resolver —
// always the attached control plane in a server built via New() (#469: the
// control plane is mandatory); nil-safe for hand-built *Server{} unit tests
// that skip New(). Unrelated to CORS since #765 (CORS is a static per-route
// policy, not sourced from Host/merchant resolution).
func (s *Server) hostMerchantResolver(ctx context.Context, host string) (merchant.ID, error) {
	if s == nil || s.controlPlane == nil {
		return merchant.ID{}, nil
	}
	return s.controlPlane.ResolveMerchantByHost(ctx, host)
}

// Handler returns the full public HTTP surface: health + user + self/customer
// + merchant + webhooks + API-key-authenticated server-to-server service routes
// (issue #222). There is no separate private/service handler — embedded hosts use
// the in-process pkg/service facade (Embedded.Service()) or this same public
// surface. It is designed to be mounted at a path prefix via http.StripPrefix.
func (s *Server) Handler() http.Handler { return s.publicHandler }
