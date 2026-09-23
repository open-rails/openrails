package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/open-rails/authkit"

	"github.com/jackc/pgx/v5/pgxpool"
	authhttp "github.com/open-rails/authkit/authhttp"
	authcore "github.com/open-rails/authkit/embedded"
	jwtkit "github.com/open-rails/authkit/jwtkit"
	"github.com/open-rails/authkit/ratelimit"
	"github.com/open-rails/authkit/verify"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// ControlPlane is OpenRails' in-process AuthKit control plane (issue #224). It
// wraps AuthKit HTTP handling and its local Runtime for identity provisioning,
// lifecycle and group/role/API-key bootstrap calls.
//
// HARD CUT (#469): the control plane is mandatory in standalone mode — the
// standalone binary always constructs it at boot and a construction failure is
// fatal. pkg/embedded hosts opt in via embed/controlplane.Attach.
type ControlPlane struct {
	cfg     *config.Config
	authSvc *authhttp.Service
	// authClient owns infrastructure; Core returns only its portable Client.
	authClient *authcore.Runtime
	client     authkit.Client
	hosted     bool
	// merchantCreation is the WithMerchantCreation config when the merchant
	// persona is opted into authkit's generated creation path (or#914); nil
	// otherwise. WrapAuthRoute attaches the directory row around the
	// generated POST /merchant, and ProvisionMerchant enforces the same
	// declared policy on in-process user-claimed slugs.
	merchantCreation *MerchantCreationConfig
	// merchantCreationPattern is merchantCreation.SlugPattern compiled and
	// anchored at construction (nil when no extra pattern is declared).
	merchantCreationPattern *regexp.Regexp
	// pool is the schema-aware wrapper used for OpenRails' own control-plane
	// queries (openrails.* tables). AuthKit's own profiles.* queries go through
	// authSvc, which holds the raw pool (#471).
	pool *db.Pool

	// delegatedVerifier validates browser-direct delegated access tokens for the
	// self-service surface (issue #222 browser tier). It is built from the same
	// issuer/audience/keys the control plane signs with, plus a self-service
	// permissions validator. See delegated.go.
	delegatedVerifier *verify.Verifier

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
	dpopRequestURL               func(*http.Request) string
	naming                       *authkit.NamingConfig
	nameAdmission                func(context.Context, authkit.NameAdmissionRequest) error
	hosted                       bool
	passwordlessLogin            bool
	passwordlessAutoRegistration bool
	email                        authcore.EmailSender
	sms                          authcore.SMSSender
	frontend                     authcore.FrontendConfig
	trustedProxies               []string
	cloudflareProxies            []string
	directPeerIP                 bool
	rateLimitOverrides           map[string]ratelimit.Limit
	redis                        *redis.Client
	merchantCreation             *MerchantCreationConfig
}

// Option configures the control plane for embedding hosts.
type Option func(*options)

// WithHostedPosture opens AuthKit registration and mounts the full AuthKit API.
// Standalone never passes this; hosted products such as openrails-saas opt in
// through embed/controlplane.
func WithHostedPosture() Option {
	return func(o *options) {
		o.hosted = true
	}
}

// WithMerchantCreation opts the merchant persona into authkit's generated
// instance-creation path (ak#263, or#914): merchant slugs claimed by USERS —
// the hosted "registration is provisioning" flow — go through
// CreateInstanceForSubject, so the slug pattern, the reserved-slug list
// (merchant.ReservedHostedSlugs plus cfg.ReservedSlugs), and the host
// admission cost gate all apply automatically, and authkit mounts
// POST /merchant with its own per-IP/per-user velocity limits. Standalone
// never passes this.
func WithMerchantCreation(cfg MerchantCreationConfig) Option {
	return func(o *options) {
		o.merchantCreation = &cfg
	}
}

// WithPasswordless enables AuthKit's contact-based passwordless login policy.
// autoRegistration additionally lets a verified unknown contact create a
// no-password user during confirmation. Both remain off unless an embedding
// host explicitly opts in through embed/controlplane.Options.
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

// WithTrustedProxies overrides the configured reverse-proxy CIDRs used by
// AuthKit's client-IP resolver. Production requires an explicit proxy or direct
// peer posture so clients do not accidentally share a proxy's rate-limit bucket.
func WithTrustedProxies(cidrs []string) Option {
	return func(o *options) { o.trustedProxies = append([]string(nil), cidrs...) }
}

// WithCloudflareProxies overrides the configured Cloudflare egress CIDRs
// (ak#298). Only these peers may assert CF-Connecting-IP; declare them solely
// where Cloudflare fronts the origin.
func WithCloudflareProxies(cidrs []string) Option {
	return func(o *options) { o.cloudflareProxies = append([]string(nil), cidrs...) }
}

// WithDirectPeerIP declares that AuthKit receives the client's direct connection.
func WithDirectPeerIP() Option {
	return func(o *options) { o.directPeerIP = true }
}

// WithRateLimitOverrides overlays bucket-specific limits onto AuthKit's
// built-in rate-limit defaults (authhttp.Config.RateLimits; #743 task 3). Keys
// are AuthKit's exported bucket names (authhttp.RLPasswordLogin, etc.); unset
// buckets keep AuthKit's default. Never replaces the whole policy —
// authhttp.Config.Limiter/DisableRateLimiting remain the (unforwarded,
// advanced-only) full-replacement seam.
func WithRateLimitOverrides(overrides map[string]ratelimit.Limit) Option {
	return func(o *options) {
		o.rateLimitOverrides = overrides
	}
}

// WithRedis wires a Redis client into the AuthKit engine's ephemeral store
// (embedded.Deps.Redis) — #753. authhttp.New reuses this SAME client for its
// own OIDC/SIWS state caches and rate limiter, so this one call satisfies both
// layers' Redis needs. Without Redis, the host must explicitly permit the
// in-memory ephemeral store through AuthConfig.AllowMemory (ak#305/#314).
func WithRedis(rd *redis.Client) Option {
	return func(o *options) {
		o.redis = rd
	}
}

// clientIPPosture resolves the authhttp client-IP declaration (ak#299): host
// options override config, direct-peer excludes proxy lists, and an
// undeclared posture refuses to boot rather than sharing one rate-limit
// bucket behind an unknown proxy.
func clientIPPosture(cfg *config.Config, auth *hostconfig.AuthConfig, options options) (authhttp.Config, error) {
	proxies := cfg.TrustedProxies
	if len(options.trustedProxies) > 0 {
		proxies = options.trustedProxies
	}
	cloudflare := cfg.CloudflareProxies
	if len(options.cloudflareProxies) > 0 {
		cloudflare = options.cloudflareProxies
	}
	directPeer := auth.DirectPeerIP || options.directPeerIP
	switch {
	case directPeer && (len(proxies) > 0 || len(cloudflare) > 0):
		return authhttp.Config{}, errors.New("controlplane: direct_peer_ip conflicts with trusted_proxies/cloudflare_proxies")
	case !directPeer && len(proxies) == 0 && len(cloudflare) == 0:
		return authhttp.Config{}, errors.New("controlplane: an explicit client-IP posture is required: set auth.direct_peer_ip=true, or trusted_proxies / cloudflare_proxies")
	}
	return authhttp.Config{
		TrustedProxies:    proxies,
		CloudflareProxies: cloudflare,
		DirectPeerIP:      directPeer,
	}, nil
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
// control plane. Inline key material from hostconfig.AuthConfig (env
// AUTHKIT_ACTIVE_KEY_ID/AUTHKIT_ACTIVE_PRIVATE_KEY_PEM/AUTHKIT_PUBLIC_KEYS,
// read once at hostconfig.Load — #712/or#917) wins when present; otherwise it falls through to the standard
// keys.json / dev-ephemeral resolution.
func resolveControlPlaneKeySource(cfg *config.Config, auth *hostconfig.AuthConfig) (jwtkit.KeySource, error) {
	activeKeyID := strings.TrimSpace(auth.ActiveKeyID)
	activePrivateKeyPEM := strings.TrimSpace(auth.ActivePrivateKeyPEM)
	if activeKeyID != "" || activePrivateKeyPEM != "" {
		var publicKeysPEM map[string]string
		if raw := strings.TrimSpace(auth.PublicKeysJSON); raw != "" {
			if err := json.Unmarshal([]byte(raw), &publicKeysPEM); err != nil {
				return nil, fmt.Errorf("controlplane: parse auth.public_keys (AUTHKIT_PUBLIC_KEYS) JSON: %w", err)
			}
		}
		ks, err := jwtkit.NewStaticKeySourceFromPEM(activeKeyID, activePrivateKeyPEM, publicKeysPEM)
		if err != nil {
			return nil, fmt.Errorf("controlplane: load JWT keys from AUTHKIT_ACTIVE_KEY_ID/AUTHKIT_ACTIVE_PRIVATE_KEY_PEM: %w", err)
		}
		// #752: inline PEM is frozen for the process lifetime by construction —
		// unlike keys_path (FILE-watched, hot-rotates), there is no way to rotate
		// or emergency-revoke this key without a restart. Every caller gets a
		// one-time-per-boot heads-up naming the tradeoff.
		{
			log.Warnf("controlplane: signing keys loaded from inline PEM (AUTHKIT_ACTIVE_KEY_ID/AUTHKIT_ACTIVE_PRIVATE_KEY_PEM) — this key is FROZEN for the process lifetime: no hot rotation and no emergency revocation without a restart. keys_path (%s) is the FILE-watched, hot-rotating production path (#752); switch to it if you need no-restart key rotation or revocation.", jwtkit.DefaultAuthKeysPath)
		}
		return ks, nil
	}
	return jwtkit.ResolveKeySource(authKeysPath(auth), auth.AllowEphemeralSigningKey, nil)
}

// authKeysPath is the directory AuthKit scans for key material. It holds BOTH
// keys.json and totp.key (#148), so it has to be handed to authkit as well as to
// the signing-key resolver: totpKeysDir reads Keys.Path whether or not a
// KeySource is supplied. Leaving it unset pinned TOTP to the /vault/auth default
// — a directory a host-run (non-container) engine does not have — so TOTP was
// permanently unavailable and every RequiresMFA role, including the root owner
// the bootstrap seeds, could never finish enrolment.
func authKeysPath(auth *hostconfig.AuthConfig) string {
	if p := strings.TrimSpace(auth.KeysPath); p != "" {
		return p
	}
	return jwtkit.DefaultAuthKeysPath
}

// New builds the OpenRails-owned AuthKit control plane from config and a pgx
// pool. The pool must point at the database that holds AuthKit's `profiles.*`
// schema (in self-hosted mode this is the same database OpenRails uses). The
// caller owns the pool lifecycle.
//
// The control plane is mandatory in standalone mode (#469): every input is
// required and a failure here is a boot failure, never a silent downgrade.
func New(ctx context.Context, cfg *config.Config, auth *hostconfig.AuthConfig, pool *pgxpool.Pool, opts ...Option) (_ *ControlPlane, retErr error) {
	if cfg == nil || auth == nil {
		return nil, errors.New("controlplane: auth.issuer is required (the control plane is mandatory in standalone mode, #469)")
	}
	if err := auth.ValidateTransport(); err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, errors.New("controlplane: pgx pool is required")
	}

	issuer := strings.TrimSpace(auth.Issuer)
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
	// call.) Discovery (#712/#231: env is read ONLY at hostconfig.Load, never here or
	// inside authkit): inline auth.active_key_id/active_private_key_pem (from
	// AUTHKIT_ACTIVE_KEY_ID/AUTHKIT_ACTIVE_PRIVATE_KEY_PEM) wins; else
	// /vault/auth/keys.json;
	// else (dev only) an ephemeral dev key.
	//
	// Mint-disabled explicitly chooses verify-only. All other construction
	// requires a real signing key or explicit permission for an ephemeral key.
	var keySource jwtkit.KeySource
	verifyOnly := false
	if auth.MintDisabled {
		verifyOnly = true
		log.Info("controlplane: auth.mint_disabled=true; running VERIFY-ONLY by declared posture (token minting disabled)")
	} else {
		ks, keyErr := resolveControlPlaneKeySource(cfg, auth)
		switch {
		case keyErr == nil:
			keySource = ks
		default:
			return nil, fmt.Errorf("controlplane: signing key discovery failed (declare auth.mint_disabled=true if verify-only is intentional, #748): %w", keyErr)
		}
	}

	options := newOptions(opts)
	lockedRegistration := !options.hosted

	// or#914: hosted merchant creation goes through authkit's generated
	// creation path (ak#263) — the merchant persona opts in and the host cost
	// gate is wired as the WithInstanceAdmission seam below.
	rbac := Groups()
	var merchantCreationPattern *regexp.Regexp
	if options.merchantCreation != nil {
		rbac = withMerchantCreation(rbac, *options.merchantCreation)
		if pat := strings.TrimSpace(options.merchantCreation.SlugPattern); pat != "" {
			re, perr := regexp.Compile("^(?:" + pat + ")$")
			if perr != nil {
				return nil, fmt.Errorf("controlplane: merchant creation slug pattern: %w", perr)
			}
			merchantCreationPattern = re
		}
	}

	naming := auth.Naming
	if options.naming != nil {
		naming = *options.naming
	}
	coreCfg := authcore.Config{
		Naming: naming,
		Keys: authcore.KeysConfig{
			Source:     keySource,
			Path:       authKeysPath(auth),
			VerifyOnly: verifyOnly,
		},
		Token: authcore.TokenConfig{
			Issuer:            issuer,
			IssuedAudiences:   []string{billingauth.TokenAudience},
			ExpectedAudiences: []string{billingauth.TokenAudience},
		},
		Frontend: resolveFrontendConfig(issuer, options.frontend),
		APIKeys:  authcore.APIKeysConfig{Prefix: APIKeyPrefix},
		// ak#314: authkit has no environment classifier; the dev-rig
		// relaxations are explicit flags, all off by default. OpenRails keeps
		// explicit local exceptions and passes them onto
		// them here: the in-memory ephemeral store (no Redis), private-network
		// JWKS for the local sandbox issuer, and missing senders (the engine
		// hands codes back instead of delivering). Omitted permissions remain
		// fail-closed: Redis required, public JWKS only, senders required.
		Ephemeral:    authcore.EphemeralConfig{AllowMemory: auth.AllowMemory},
		Applications: authcore.ApplicationsConfig{AllowPrivateNetworkJWKS: auth.AllowPrivateNetworkJWKS},
		// HARD CUT (#567): OpenRails declares two FLAT top-level permission-group
		// personas under `root` — `merchant` (owner/support/viewer, `merchant:*`)
		// and `customer` (owner/member, `customer:*`). There is no merchant coupling
		// outside permission groups. Each type's `owner` role is auto-seeded
		// (= `<type>:*`), so OwnerOwnsAppResources is obsolete (every owner holds
		// its own namespace directly; the flat case needs no cross-namespace grant).
		RBAC: rbac,
		// Private standalone posture: no public user self-registration. Embedded
		// privileged Client calls (CreatePermissionGroup/OperatorAssignGroupRole/MintAPIKey)
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
			AllowMissingSenders:          auth.AllowMissingSenders,
		},
	}

	// Engine dependencies (ak#314 embedded.Deps). Host-owned senders (#738)
	// and the app's Redis client (#753) are wired here once; the HTTP
	// constructor reuses that Redis for its OIDC/SIWS caches and rate limiter.
	deps := authcore.Deps{
		River:    authcore.RiverFromHost(),
		Postgres: pool,
		Redis:    options.redis,
		Email:    options.email,
		SMS:      options.sms,
		NameAdmission: func(ctx context.Context, request authkit.NameAdmissionRequest) error {
			if request.OwnerKind == "group" && request.Persona == CustomerType && request.Operation == authkit.NameRename {
				return authkit.ErrGroupSlugApplicationManaged
			}
			if options.nameAdmission != nil {
				return options.nameAdmission(ctx, request)
			}
			return nil
		},
	}
	if options.merchantCreation != nil && options.merchantCreation.Admission != nil {
		// ak#263 host admission seam: the predicate receives the normalized
		// slug. Personas other than merchant never reach the host gate —
		// openrails enables creation on the merchant persona only.
		admit := options.merchantCreation.Admission
		deps.InstanceAdmission = func(ctx context.Context, group authkit.GroupRef, subject string) error {
			if group.Persona != MerchantType {
				return nil
			}
			return admit(ctx, group.Instance, subject)
		}
	}

	httpCfg, err := clientIPPosture(cfg, auth, options)
	if err != nil {
		return nil, err
	}
	httpCfg.RateLimits = options.rateLimitOverrides

	cp2 := &ControlPlane{
		cfg: cfg, hosted: options.hosted, merchantCreation: options.merchantCreation,
		merchantCreationPattern: merchantCreationPattern,
		pool:                    db.WrapPool(pool, cfg.DB.SchemaName()), issuer: issuer,
		delegatedAudiences: []string{billingauth.TokenAudience},
	}
	coreCfg.HTTP = controlPlaneHTTP{controlPlane: cp2, config: httpCfg,
		requestURL: delegatedRequestURL(auth.RequestOrigin, options.dpopRequestURL)}
	authRuntime, err := authcore.New(coreCfg, deps)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build authkit runtime: %w", err)
	}
	cp2.authClient = authRuntime
	cp2.client = authRuntime.Client()
	defer func() {
		if retErr != nil {
			authRuntime.Close()
		}
	}()

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

// Close releases the HTTP adapter and schema-bound AuthKit pool created by New.
// The host pool supplied to New remains owned by the caller.
func (c *ControlPlane) Close() {
	if c == nil {
		return
	}
	if c.authClient != nil {
		c.authClient.Close()
		c.authClient = nil
		c.authSvc = nil
		c.client = nil
	}
}

// Core returns the portable AuthKit operation Client. Runtime is kept private.
func (c *ControlPlane) Core() authkit.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// MerchantCreationEnabled reports whether the merchant persona is opted into
// authkit's generated instance-creation path (or#914, WithMerchantCreation).
func (c *ControlPlane) MerchantCreationEnabled() bool {
	return c != nil && c.merchantCreation != nil
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
	// Native identity follows the accepted access-token lifetime. Permission
	// gates still consult current group authority; machine/delegated credential
	// validation remains the verifier's responsibility.
	return auth.NewAuthenticator(auth.AuthenticatorConfig{
		Verifier:       auth.RequestVerifierFunc(c.authSvc.Verifier().VerifyRequest),
		OmitTokenRoles: true,
	})
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

func WithNaming(input authkit.NamingConfig) Option {
	return func(options *options) { options.naming = &input }
}
func WithNameAdmission(admit func(context.Context, authkit.NameAdmissionRequest) error) Option {
	return func(options *options) { options.nameAdmission = admit }
}

// WithDPoPRequestURL maps rewritten HTTP paths to the externally visible URL.
// Hosts must derive this from trusted routing configuration, never forwarded
// headers supplied by an arbitrary client.
func WithDPoPRequestURL(target func(*http.Request) string) Option {
	return func(o *options) { o.dpopRequestURL = target }
}

func delegatedRequestURL(requestOrigin string, override func(*http.Request) string) func(*http.Request) string {
	if override != nil {
		return override
	}
	base := strings.TrimRight(strings.TrimSpace(requestOrigin), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		base = ""
	}
	return func(r *http.Request) string {
		if base == "" {
			return ""
		}
		return base + r.URL.EscapedPath()
	}
}
