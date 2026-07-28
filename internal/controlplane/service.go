package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	authhttp "github.com/open-rails/authkit/authhttp"
	authcore "github.com/open-rails/authkit/embedded"
	jwtkit "github.com/open-rails/authkit/jwtkit"
	"github.com/open-rails/authkit/ratelimit"
	"github.com/open-rails/authkit/verify"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// ControlPlane is OpenRails' in-process AuthKit control plane (issue #224). It
// wraps an AuthKit http.Service (which exposes selectable route groups) and its
// underlying core.Service (used for in-process group/role/API-key bootstrap calls).
//
// HARD CUT (#469): the control plane is mandatory in standalone mode — the
// standalone binary always constructs it at boot and a construction failure is
// fatal. pkg/embedded hosts opt in via pkg/embedded/controlplane.Attach.
type ControlPlane struct {
	cfg     *config.Config
	authSvc *authhttp.Service
	// authClient is the in-process AuthKit engine the host built (client-first,
	// #142); authSvc adapts it for HTTP, Core()/the delegated verifier use it
	// directly. The server no longer vends it (.Client() was dropped).
	authClient *authcore.Client
	hosted     bool
	// pool is the schema-aware wrapper used for OpenRails' own control-plane
	// queries (openrails.* tables). AuthKit's own profiles.* queries go through
	// authSvc, which holds the raw pool (#471).
	pool *db.Pool

	// delegatedVerifier validates browser-direct delegated access tokens for the
	// self-service surface (issue #222 browser tier). It is built from the same
	// issuer/audience/keys the control plane signs with, plus a self-service
	// permissions validator. See delegated.go.
	delegatedVerifier *verify.Verifier
	// userVerifier validates first-party AuthKit user access tokens for narrow
	// OpenRails wrappers around AuthKit-generated routes. AuthKit still performs
	// the authoritative route auth inside its handler; this verifier only lets
	// OpenRails run pre-auth setup such as lazy customer-group creation.
	userVerifier *verify.Verifier

	// issuer is the control plane's token issuer (`iss`), stamped on minted tokens
	// so they verify against delegatedVerifier.
	issuer string
	// delegatedAudiences are the audiences (`aud`) stamped on minted delegated
	// access tokens. They are the control plane's accepted (expected) audiences, so
	// every minted token is accepted by delegatedVerifier (and the /v1/me gate).
	delegatedAudiences []string

	// issuerRefresh drives the activity-based TTL re-sync of delegatedVerifier's
	// issuer registry against out-of-band store writes (#852, issuer_registry.go).
	issuerRefresh issuerRegistryRefresh
}

type options struct {
	hosted                       bool
	passwordlessLogin            bool
	passwordlessAutoRegistration bool
	email                        authcore.EmailSender
	sms                          authcore.SMSSender
	frontend                     authcore.FrontendConfig
	trustedProxies               []string
	rateLimitOverrides           map[string]ratelimit.Limit
	redis                        *redis.Client
}

// Option configures the control plane for embedding hosts.
type Option func(*options)

// WithHostedPosture opens AuthKit registration and mounts the full AuthKit API.
// Standalone never passes this; hosted products such as openrails-saas opt in
// through pkg/embedded/controlplane.
func WithHostedPosture() Option {
	return func(o *options) {
		o.hosted = true
	}
}

// WithPasswordless enables AuthKit's contact-based passwordless login policy.
// autoRegistration additionally lets a verified unknown contact create a
// no-password user during confirmation. Both remain off unless an embedding
// host explicitly opts in through pkg/embedded/controlplane.AttachOptions.
func WithPasswordless(autoRegistration bool) Option {
	return func(o *options) {
		o.passwordlessLogin = true
		o.passwordlessAutoRegistration = autoRegistration
	}
}

// WithEmailSender wires a host-owned verification email sender into the AuthKit
// engine (#738). Hosted posture keeps registration verification Required, and
// authkit refuses construction when that policy has no configured sender — so a
// hosted host must supply at least one of email/SMS. Self-hosted posture never
// needs one (closed registration verifies nothing).
func WithEmailSender(sender authcore.EmailSender) Option {
	return func(o *options) {
		o.email = sender
	}
}

// WithSMSSender wires a host-owned verification SMS sender (#738); see
// WithEmailSender.
func WithSMSSender(sender authcore.SMSSender) Option {
	return func(o *options) {
		o.sms = sender
	}
}

// WithFrontend overrides authcore.Config.Frontend — the host-owned frontend
// routes AuthKit uses to build absolute emailed links (verification, password
// reset, ...). #743: without this, Frontend.BaseURL pins to the control
// plane's own token issuer, which for a hosted product is an API host that
// serves no pages — emailed verify/reset links 404. Hosted products MUST set
// this to their product frontend's origin. Absent (zero value), New keeps the
// pre-#743 default (BaseURL: issuer); every path left blank on a supplied
// override still falls back to authcore's own per-field defaults
// (VerifyPath->"/verify", PasswordResetPath->"/reset", etc).
func WithFrontend(fc authcore.FrontendConfig) Option {
	return func(o *options) {
		o.frontend = fc
	}
}

// WithTrustedProxies configures the reverse-proxy/LB CIDRs AuthKit's HTTP
// server trusts when deriving the client IP (CF-Connecting-IP / X-Forwarded-For)
// for its per-client registration/login rate limiter (#743). Without this,
// authhttp keys the limiter off the immediate TCP peer — behind any load
// balancer that is the LB's own address, so the whole product shares ONE
// registration bucket. Empty (the default) keeps authhttp's safe
// direct-RemoteAddr behavior.
func WithTrustedProxies(cidrs []string) Option {
	return func(o *options) {
		o.trustedProxies = cidrs
	}
}

// WithRateLimitOverrides overlays bucket-specific limits onto AuthKit's
// built-in rate-limit defaults (authhttp.WithRateLimitOverrides; #743 task 3,
// unblocked by authkit v0.79.0's authkit#242 merge-style override API). Keys
// are AuthKit's exported bucket names (authhttp.RLPasswordLogin, etc.);
// unset buckets keep AuthKit's default. Never replaces the whole policy —
// authhttp.WithRateLimiter/WithoutRateLimiter remain the (unforwarded,
// advanced-only) full-replacement seam.
func WithRateLimitOverrides(overrides map[string]ratelimit.Limit) Option {
	return func(o *options) {
		o.rateLimitOverrides = overrides
	}
}

// WithRedis wires a Redis client into the AuthKit engine's ephemeral store
// (authcore.WithRedis) — #753. authhttp.NewServer (authkit v0.79.0, #210)
// automatically reuses this SAME client for its own OIDC/SIWS state caches
// and rate limiter, so this one call satisfies both layers' Redis needs.
// Required outside development: authhttp.NewServer hard-fails construction
// when Environment is non-development and no Redis-backed ephemeral store
// was wired.
func WithRedis(rd *redis.Client) Option {
	return func(o *options) {
		o.redis = rd
	}
}

func newOptions(opts []Option) options {
	var out options
	for _, opt := range opts {
		if opt != nil {
			opt(&out)
		}
	}
	return out
}

// registrationMode maps the lock flag to an AuthKit registration mode.
//
// Standalone passes locked=true, so registration is closed and no config/env
// knob can open it.
// Embedded hosts can opt into locked=false with WithHostedPosture.
func registrationMode(locked bool) authcore.RegistrationMode {
	if locked {
		return authcore.RegistrationModeClosed
	}
	return authcore.RegistrationModeOpen
}

// registrationVerification maps the lock flag to a verification policy: closed
// registration has nothing to verify (none); hosted (open) keeps the secure
// required default and must configure a sender.
func registrationVerification(locked bool) authcore.RegistrationVerificationPolicy {
	if locked {
		return authcore.RegistrationVerificationNone
	}
	return authcore.RegistrationVerificationRequired
}

// resolveFrontendConfig resolves authcore.Config.Frontend (#743): an explicit
// host override (WithFrontend) wins outright — a host that supplies ANY
// FrontendConfig owns the whole struct, and authcore itself defaults every
// field the host leaves blank (BaseURL->issuer, VerifyPath->"/verify",
// PasswordResetPath->"/reset", ...). Absent an override, New keeps the
// pre-#743 default of pinning BaseURL at the control-plane issuer, made
// explicit here rather than relying on authcore's own BaseURL-empty fallback.
func resolveFrontendConfig(issuer string, override authcore.FrontendConfig) authcore.FrontendConfig {
	if override != (authcore.FrontendConfig{}) {
		return override
	}
	return authcore.FrontendConfig{BaseURL: issuer}
}

// resolveControlPlaneKeySource builds the JWT signing KeySource for the
// control plane. Inline key material from config.Config.Auth (env
// ACTIVE_KEY_ID/ACTIVE_PRIVATE_KEY_PEM/PUBLIC_KEYS, read once at config.Load —
// #712) wins when present; otherwise it falls through to the standard
// keys.json / dev-ephemeral resolution.
func resolveControlPlaneKeySource(cfg *config.Config) (jwtkit.KeySource, error) {
	activeKeyID := strings.TrimSpace(cfg.Auth.ActiveKeyID)
	activePrivateKeyPEM := strings.TrimSpace(cfg.Auth.ActivePrivateKeyPEM)
	if activeKeyID != "" || activePrivateKeyPEM != "" {
		var publicKeysPEM map[string]string
		if raw := strings.TrimSpace(cfg.Auth.PublicKeysJSON); raw != "" {
			if err := json.Unmarshal([]byte(raw), &publicKeysPEM); err != nil {
				return nil, fmt.Errorf("controlplane: parse auth.public_keys (PUBLIC_KEYS) JSON: %w", err)
			}
		}
		ks, err := jwtkit.NewStaticKeySourceFromPEM(activeKeyID, activePrivateKeyPEM, publicKeysPEM)
		if err != nil {
			return nil, fmt.Errorf("controlplane: load JWT keys from ACTIVE_KEY_ID/ACTIVE_PRIVATE_KEY_PEM: %w", err)
		}
		// #752: inline PEM is frozen for the process lifetime by construction —
		// unlike keys_path (FILE-watched, hot-rotates), there is no way to rotate
		// or emergency-revoke this key without a restart. Development is exempt
		// (short-lived, disposable processes); every other environment gets a
		// loud, one-time-per-boot heads-up naming the tradeoff.
		if !cfg.IsDev() {
			log.Warnf("controlplane: signing keys loaded from inline PEM (ACTIVE_KEY_ID/ACTIVE_PRIVATE_KEY_PEM) outside development — this key is FROZEN for the process lifetime: no hot rotation and no emergency revocation without a restart. keys_path (%s) is the FILE-watched, hot-rotating production path (#752); switch to it if you need no-restart key rotation or revocation.", jwtkit.DefaultAuthKeysPath)
		}
		return ks, nil
	}
	keysPath := strings.TrimSpace(cfg.Auth.KeysPath)
	if keysPath == "" {
		keysPath = jwtkit.DefaultAuthKeysPath
	}
	return jwtkit.ResolveKeySource(keysPath, cfg.IsDev())
}

// New builds the OpenRails-owned AuthKit control plane from config and a pgx
// pool. The pool must point at the database that holds AuthKit's `profiles.*`
// schema (in self-hosted mode this is the same database OpenRails uses). The
// caller owns the pool lifecycle.
//
// The control plane is mandatory in standalone mode (#469): every input is
// required and a failure here is a boot failure, never a silent downgrade.
func New(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, opts ...Option) (*ControlPlane, error) {
	if cfg == nil || cfg.Auth == nil {
		return nil, errors.New("controlplane: auth.issuer is required (the control plane is mandatory in standalone mode, #469)")
	}
	if pool == nil {
		return nil, errors.New("controlplane: pgx pool is required")
	}

	issuer := strings.TrimSpace(cfg.Auth.Issuer)
	if issuer == "" {
		return nil, errors.New("controlplane: auth.issuer is required")
	}

	// (authkit issue 60) Group routes are always a supported primitive — no group-mode
	// config or core flag.

	// Build the signing KeySource ONCE and inject it into the core config, so the
	// active signer the control plane MINTS with and the public keys the delegated
	// verifier TRUSTS are guaranteed to be the same key. (When Keys is left nil,
	// core.NewFromConfig auto-discovers internally and we'd have no handle on the
	// active signer; in dev it could even generate a different key on a second
	// call.) Discovery (#712/#231: env is read ONLY at config.Load, never here or
	// inside authkit): inline auth.active_key_id/active_private_key_pem (from
	// ACTIVE_KEY_ID/ACTIVE_PRIVATE_KEY_PEM) wins; else /vault/auth/keys.json;
	// else (dev only) an ephemeral dev key.
	//
	// The signing key is OPTIONAL (#527/#87): a control plane with no key runs
	// as a pure VERIFIER instead of failing the boot — it verifies inbound
	// host-app delegated tokens and serves RBAC, but cannot MINT tokens (mint
	// paths return authkit ErrMissingSigner). But verify-only must be a
	// DECLARED posture, not stumbled into (#748): auth.mint_disabled=true
	// says so explicitly and skips key discovery entirely (there is nothing to
	// discover — minting is off by intent). Without that flag, a signing-key
	// discovery error used to silently downgrade to verify-only with a warn,
	// indistinguishable from an outage, with mint failures only surfacing at
	// request time. Now a discovery error is a construction (boot) failure
	// outside development (mirrors the #667 encryption-posture gate);
	// development keeps the original warn-and-continue so a from-scratch dev
	// boot with no keys.json still runs on its ephemeral dev key path (which
	// itself never errors — see jwtkit.ResolveKeySource).
	var keySource jwtkit.KeySource
	verifyOnly := false
	if cfg.Auth.MintDisabled {
		verifyOnly = true
		log.Info("controlplane: auth.mint_disabled=true; running VERIFY-ONLY by declared posture (token minting disabled)")
	} else {
		ks, keyErr := resolveControlPlaneKeySource(cfg)
		switch {
		case keyErr == nil:
			keySource = ks
		case cfg.IsDev():
			log.WithError(keyErr).Warn("controlplane: no signing key discovered; running VERIFY-ONLY (token minting disabled) — development only; declare auth.mint_disabled=true to make this posture explicit outside development (#748)")
			verifyOnly = true
		default:
			return nil, fmt.Errorf("controlplane: signing key discovery failed outside development (declare auth.mint_disabled=true if verify-only is intentional, #748): %w", keyErr)
		}
	}

	options := newOptions(opts)
	lockedRegistration := !options.hosted

	coreCfg := authcore.Config{
		Keys: authcore.KeysConfig{
			Source:     keySource,
			VerifyOnly: verifyOnly,
		},
		Token: authcore.TokenConfig{
			Issuer:            issuer,
			IssuedAudiences:   []string{billingauth.TokenAudience},
			ExpectedAudiences: []string{billingauth.TokenAudience},
		},
		Frontend:    resolveFrontendConfig(issuer, options.frontend),
		APIKeys:     authcore.APIKeysConfig{Prefix: APIKeyPrefix},
		Environment: strings.TrimSpace(cfg.Env),
		// HARD CUT (#567): OpenRails declares two FLAT top-level permission-group
		// personas under `root` — `merchant` (owner/support/viewer, `merchant:*`)
		// and `customer` (owner/member, `customer:*`). There is no merchant coupling
		// outside permission groups. Each type's `owner` role is auto-seeded
		// (= `<type>:*`), so OwnerOwnsAppResources is obsolete (every owner holds
		// its own namespace directly; the flat case needs no cross-namespace grant).
		RBAC: Groups(),
		// Private standalone posture: no public user self-registration. Embedded
		// bootstrap/core calls (CreatePermissionGroup/Genesis().AssignGroupRole/MintAPIKey)
		// are unaffected. Hosted products opt in with WithHostedPosture; no
		// config/env knob opens this in standalone.
		// Verification set EXPLICITLY: authkit v0.76.0 defaults unset to
		// "required" (its doc says "none" — code wins), which refuses boot with
		// no sender. Locked registration has nothing to verify → none; hosted
		// keeps required and must configure an email/SMS sender
		// (WithEmailSender/WithSMSSender, #738).
		Registration: authcore.RegistrationConfig{
			NativeUserMode:               registrationMode(lockedRegistration),
			Verification:                 registrationVerification(lockedRegistration),
			PasswordlessLogin:            options.passwordlessLogin,
			PasswordlessAutoRegistration: options.passwordlessAutoRegistration,
		},
	}

	// Host-owned verification senders (#738) are engine dependencies, wired as
	// authkit functional options. authkit enforces the required-policy/sender
	// pairing itself at HTTP-server construction below — no downgrade here.
	var coreOpts []authcore.Option
	if options.email != nil {
		coreOpts = append(coreOpts, authcore.WithEmailSender(options.email))
	}
	if options.sms != nil {
		coreOpts = append(coreOpts, authcore.WithSMSSender(options.sms))
	}
	// #753: wire the engine's Redis client as AuthKit's ephemeral store.
	// authhttp.NewServer below (authkit v0.79.0, #210) automatically reuses
	// THIS SAME client for its own OIDC/SIWS caches and rate limiter — one
	// wire satisfies both layers, and is REQUIRED outside development (see
	// authhttp.NewServer's validate()).
	if options.redis != nil {
		coreOpts = append(coreOpts, authcore.WithRedis(options.redis))
	}

	// Client-first construction (#142): build the AuthKit engine, then adapt it
	// with the HTTP server. Core() / the delegated verifier hold this client.
	authClient, err := authcore.New(coreCfg, pool, coreOpts...)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build authkit client: %w", err)
	}
	// HTTP-layer options (#743): trusted-proxy CIDRs so the per-client
	// registration/login rate limiter keys on the real client behind a load
	// balancer instead of the LB's own peer address; per-bucket rate-limit
	// overrides layered onto AuthKit's defaults.
	var authHTTPOpts []authhttp.Option
	if len(options.trustedProxies) > 0 {
		authHTTPOpts = append(authHTTPOpts, authhttp.WithTrustedProxies(options.trustedProxies...))
	}
	if len(options.rateLimitOverrides) > 0 {
		authHTTPOpts = append(authHTTPOpts, authhttp.WithRateLimitOverrides(options.rateLimitOverrides))
	}
	authSvc, err := authhttp.NewServer(authClient, authHTTPOpts...)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build authkit service: %w", err)
	}

	// The verifier trusts exactly what coreCfg above declared (issuer,
	// audiences, API-key prefix) — read the local values instead of an
	// accessor so mint and verify cannot drift.
	userVerifier := verify.NewVerifier(
		verify.WithSkew(5*time.Second),
		verify.WithAPIKeyPrefix(APIKeyPrefix),
		verify.WithSSRFGuard(),
	)
	if err := userVerifier.AddIssuer(issuer, []string{billingauth.TokenAudience}, verify.IssuerOptions{
		RawKeys: authClient.PublicKeysByKID(),
		IsLocal: true,
	}); err != nil {
		return nil, fmt.Errorf("controlplane: build user verifier: %w", err)
	}
	userVerifier.WithService(authClient)

	// Build the browser-direct delegated-access-token verifier (#222 browser
	// tier). It accepts registered delegated tokens with the canonical
	// `openrails` audience. Customer delegated self-service JWTs may be
	// permissionless; any supplied permissions are bounded by the signing remote
	// application's stored authority in AuthKit.
	delegatedVerifier, err := newDelegatedVerifier(authClient, APIKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build delegated verifier: %w", err)
	}

	// Wire AuthKit's core as the verifier's enrichment + remote_application source
	// so it can lazy-load any single issuer on first use (#481). *core.Service
	// satisfies verify.Enricher directly under the permission-group model (#567):
	// remote-app authority is resolved as the additive walk-up of the app's group
	// roles, kept as raw grant tokens; OpenRails gates them with its own
	// namespace-glob matcher on every credential type (#565).
	delegatedVerifier.WithService(authClient)

	cp2 := &ControlPlane{
		cfg:                cfg,
		authSvc:            authSvc,
		authClient:         authClient,
		hosted:             options.hosted,
		pool:               db.WrapPool(pool, cfg.DB.SchemaName()),
		delegatedVerifier:  delegatedVerifier,
		userVerifier:       userVerifier,
		issuer:             issuer,
		delegatedAudiences: []string{billingauth.TokenAudience},
	}

	// Load AuthKit's ACTIVE remote_applications into the multi-issuer verifier
	// (#481: standalone JWKS trust is AuthKit's remote_application registry, #74).
	// A load failure must NOT take down startup: unreachable JWKS is handled
	// lazily/fail-closed per token at verify time, and the verifier lazy-loads any
	// single issuer on first use. So we log and continue.
	if err := cp2.loadRemoteApplications(ctx); err != nil {
		log.WithError(err).Warn("controlplane: initial remote_application load failed; delegated tokens fail closed / lazy-load until next load")
	}

	return cp2, nil
}

// Core returns the underlying AuthKit core service used for in-process
// group/role/API key operations. The return type is the concrete
// *authcore.Client (not the authkit.Client interface): it satisfies
// authkit.Client in full (every existing Core()-based call site keeps
// compiling unchanged) AND additionally exposes .Genesis() (authkit v0.79.0,
// #241) — the unchecked bootstrap/migration mutators (AssignGroupRole,
// AssignRoleBySlug, RemoveRoleBySlug, RemoveGroupSubject) that authkit
// deliberately dropped from the swappable Client interface. Bootstrap/seed/
// lazy-materialization code (internal/controlplane/bootstrap.go,
// customer_group.go, internal/bootstrap/merchant_manifest.go, the integration
// harness) calls Core().Genesis().AssignGroupRole(...) etc.; runtime request
// handlers must use the actor-checked *As methods on the Client interface
// instead (never Genesis()).
func (c *ControlPlane) Core() *authcore.Client {
	if c == nil {
		return nil
	}
	return c.authClient
}

// AuthService returns the underlying AuthKit http.Service (for route mounting).
func (c *ControlPlane) AuthService() *authhttp.Service {
	if c == nil {
		return nil
	}
	return c.authSvc
}

// UserAuthenticator returns the in-process billingauth.Authenticator for a host
// embedding this control plane (#739): the host's own HTTP routes authenticate
// bearer tokens against the SAME verifier state the control plane mints and
// verifies with (issuer, audiences, API-key prefix, in-memory signing keys,
// core-service enrichment) — no JWKS HTTP fetch, so mint and verify cannot
// drift. It is exactly the authenticator the standalone server wires for its
// user routes.
//
// Scope: hosts embedding the control plane use THIS for their own routes.
// pkg/embedded/authkit.NewVerifierAuthenticator remains for verifying REMOTE
// issuers over JWKS; a JWKS HTTP route exists purely for external verifiers.
// Returns nil when the control plane or its verifier is absent.
func (c *ControlPlane) UserAuthenticator() billingauth.Authenticator {
	if c == nil || c.authSvc == nil || c.authSvc.Verifier() == nil {
		return nil
	}
	return auth.NewAuthenticator(c.authSvc.Verifier())
}

// Pool returns the control plane's schema-aware pgx pool (the pool holding the
// openrails.* control-plane schema). Used by the tenancy lifecycle/secret-store
// service (issue #225), which owns the same OpenRails-owned control-plane state.
// SQL run on it is schema-rewritten to the configured schema (#471); call
// Pool().Raw() for the underlying pool when an API needs it verbatim. nil when
// the control plane is disabled.
func (c *ControlPlane) Pool() *db.Pool {
	if c == nil {
		return nil
	}
	return c.pool
}

// SelfHostedPosture reports whether this control plane mounts only the
// intentional AuthKit route groups (RoutePublic + RouteSession + RouteUser).
// Standalone construction never passes WithHostedPosture, so private OpenRails
// remains locked by default; embedded hosts can opt into hosted posture in code.
func (c *ControlPlane) SelfHostedPosture() bool {
	if c == nil {
		return true
	}
	return !c.hosted
}
