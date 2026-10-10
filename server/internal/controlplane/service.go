package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	userauth "github.com/open-rails/openrails/server/internal/auth"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// ControlPlane is the standalone server's multi-merchant control plane over
// its own AuthKit client, which holds identities, merchant groups and
// credentials: merchant provisioning and names, teams, API keys, federated
// grants, the OAuth resource server and fleet aggregates. Only package server
// builds one; the embedded engine has none.
type ControlPlane struct {
	client *authkit.Client
	// registration is AuthKit's native self-registration mode; closed unless
	// the host opens it (WithRegistration).
	registration iam.RegistrationMode
	// localSignIn mounts AuthKit's sign-in surface (WithLocalSignIn).
	localSignIn bool
	// merchantCreation is the hosted policy for user-claimed merchant names
	// (WithMerchantCreation, or#914); nil otherwise.
	merchantCreation *MerchantCreationConfig
	// merchantCreationPattern is merchantCreation.SlugPattern compiled and
	// anchored at construction (nil when no extra pattern is declared).
	merchantCreationPattern *regexp.Regexp
	// naming is the rename policy for merchant names (#1106).
	naming config.NamingPolicy
	// pool is the schema-aware wrapper for OpenRails' own tables (#471).
	pool *db.Pool
	// authPrefix is the path prefix AuthKit's JSON API is served beneath.
	authPrefix string

	// users authenticates the control plane's own user tokens; the actor of
	// a request-driven permission check comes from its verification.
	users *userauth.Authenticator
	// resource verifies trusted issuers' RFC 9068 access tokens (#1140); nil
	// accepts none.
	resource *resourceServer
}

type options struct {
	naming                       *config.NamingConfig
	nameAdmission                func(context.Context, iam.NameAdmissionRequest) error
	registration                 iam.RegistrationMode
	passwordlessLogin            bool
	localSignIn                  bool
	passwordlessAutoRegistration bool
	email                        authkit.EmailSender
	sms                          authkit.SMSSender
	frontend                     authkit.FrontendConfig
	directPeerIP                 bool
	rateLimitOverrides           map[string]authkit.RateLimit
	redis                        *redis.Client
	merchantCreation             *MerchantCreationConfig
	resourceServer               *hostconfig.ResourceServerConfig
}

// Option configures the control plane.
type Option func(*options)

// WithRegistration sets AuthKit's native self-registration mode. Empty is
// closed: a self-hosted server registers nobody; hosted products open it.
func WithRegistration(mode iam.RegistrationMode) Option {
	return func(o *options) { o.registration = mode }
}

// WithMerchantCreation declares the hosted policy for merchant names claimed
// by users (or#914): the reserved names (billing.ReservedMerchantSlugs plus
// cfg.ReservedSlugs), the creation pattern and the admission cost gate.
// Standalone never passes this.
func WithMerchantCreation(cfg MerchantCreationConfig) Option {
	return func(o *options) { o.merchantCreation = &cfg }
}

// WithLocalSignIn serves sign-in to the control plane's own accounts.
func WithLocalSignIn() Option {
	return func(o *options) { o.localSignIn = true }
}

// WithPasswordless enables AuthKit's contact-based passwordless login.
// autoRegistration also lets a verified unknown contact create a no-password
// user during confirmation.
func WithPasswordless(autoRegistration bool) Option {
	return func(o *options) {
		o.passwordlessLogin = true
		o.passwordlessAutoRegistration = autoRegistration
	}
}

// WithEmailSender wires the host's email delivery (#738). Open or
// invite-only registration verifies contacts, so it needs an email or SMS
// sender.
func WithEmailSender(sender authkit.EmailSender) Option {
	return func(o *options) { o.email = sender }
}

// WithSMSSender wires the host's SMS delivery (#738); see WithEmailSender.
func WithSMSSender(sender authkit.SMSSender) Option {
	return func(o *options) { o.sms = sender }
}

// WithFrontend sets the host-owned frontend routes AuthKit builds emailed
// links from (#743). Hosted products MUST set BaseURL to their frontend's
// origin: the default, the control plane's issuer, serves no pages.
func WithFrontend(fc authkit.FrontendConfig) Option {
	return func(o *options) { o.frontend = fc }
}

// WithDirectPeerIP declares that AuthKit receives the client's direct connection.
func WithDirectPeerIP() Option {
	return func(o *options) { o.directPeerIP = true }
}

// WithRateLimitOverrides overlays bucket limits onto AuthKit's defaults
// (authkit.DefaultRateLimits, #743); unknown buckets are refused.
func WithRateLimitOverrides(overrides map[string]authkit.RateLimit) Option {
	return func(o *options) { o.rateLimitOverrides = overrides }
}

// WithRedis shares AuthKit's rate-limit counters across replicas. Without it
// the host must declare a single replica (auth.allow_memory).
func WithRedis(rd *redis.Client) Option {
	return func(o *options) { o.redis = rd }
}

// WithNaming overrides auth.naming, the site naming policy.
func WithNaming(n config.NamingConfig) Option {
	return func(o *options) { o.naming = &n }
}

// WithNameAdmission is the host's side-effect-free username policy.
func WithNameAdmission(admit func(context.Context, iam.NameAdmissionRequest) error) Option {
	return func(o *options) { o.nameAdmission = admit }
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

// clientIPPosture resolves the client-IP declaration (ak#299): AuthKit trusts
// exactly the engine's proxies, since both serve behind the same ones;
// direct-peer excludes proxy lists, and an undeclared posture refuses to boot
// rather than sharing one rate-limit bucket behind an unknown proxy.
func clientIPPosture(cfg *config.Config, auth *hostconfig.AuthConfig, options options) (authkit.HTTPConfig, error) {
	proxies, cloudflare := cfg.TrustedProxies, cfg.CloudflareProxies
	directPeer := auth.DirectPeerIP || options.directPeerIP
	switch {
	case directPeer && (len(proxies) > 0 || len(cloudflare) > 0):
		return authkit.HTTPConfig{}, errors.New("controlplane: direct_peer_ip conflicts with trusted_proxies/cloudflare_proxies")
	case !directPeer && len(proxies) == 0 && len(cloudflare) == 0:
		return authkit.HTTPConfig{}, errors.New("controlplane: an explicit client-IP posture is required: set auth.direct_peer_ip=true, or trusted_proxies / cloudflare_proxies")
	}
	return authkit.HTTPConfig{TrustedProxies: proxies, CloudflareProxies: cloudflare, DirectPeerIP: directPeer}, nil
}

// Registration policy (#469): closed registers nobody and verifies nothing;
// open and invite-only registration require verified contacts, so they need a
// sender.
func registration(options options, auth *hostconfig.AuthConfig) authkit.RegistrationConfig {
	reg := authkit.RegistrationConfig{
		NativeUserMode:               registrationMode(options.registration),
		Verification:                 iam.RegistrationVerificationNone,
		PasswordlessLogin:            options.passwordlessLogin,
		PasswordlessAutoRegistration: options.passwordlessAutoRegistration,
		AllowMissingSenders:          auth.AllowMissingSenders,
	}
	if reg.NativeUserMode != iam.RegistrationModeClosed {
		reg.Verification = iam.RegistrationVerificationRequired
	}
	return reg
}

// registrationMode resolves the declared mode; empty is closed.
func registrationMode(mode iam.RegistrationMode) iam.RegistrationMode {
	if mode == "" {
		return iam.RegistrationModeClosed
	}
	return mode
}

// ValidateRegistrationMode refuses anything but AuthKit's three modes.
func ValidateRegistrationMode(mode iam.RegistrationMode) error {
	switch mode {
	case "", iam.RegistrationModeOpen, iam.RegistrationModeInviteOnly, iam.RegistrationModeClosed:
		return nil
	}
	return fmt.Errorf("controlplane: registration %q is not one of open, invite_only, closed", mode)
}

// inlineKeySource is the signing key from inline PEM (AUTHKIT_ACTIVE_KEY_ID /
// AUTHKIT_ACTIVE_PRIVATE_KEY_PEM / AUTHKIT_PUBLIC_KEYS, read once at
// hostconfig.Load, #712/or#917), or nil to resolve auth.keys_path.
func inlineKeySource(auth *hostconfig.AuthConfig) (keys.Source, error) {
	activeKeyID := strings.TrimSpace(auth.ActiveKeyID)
	activePrivateKeyPEM := strings.TrimSpace(auth.ActivePrivateKeyPEM)
	if activeKeyID == "" && activePrivateKeyPEM == "" {
		return nil, nil
	}
	var publicKeysPEM map[string]string
	if raw := strings.TrimSpace(auth.PublicKeysJSON); raw != "" {
		if err := json.Unmarshal([]byte(raw), &publicKeysPEM); err != nil {
			return nil, fmt.Errorf("controlplane: parse auth.public_keys (AUTHKIT_PUBLIC_KEYS) JSON: %w", err)
		}
	}
	ks, err := keys.StaticFromPEM(activeKeyID, activePrivateKeyPEM, publicKeysPEM)
	if err != nil {
		return nil, fmt.Errorf("controlplane: load JWT keys from AUTHKIT_ACTIVE_KEY_ID/AUTHKIT_ACTIVE_PRIVATE_KEY_PEM: %w", err)
	}
	// #752: an inline key cannot rotate or be revoked without a restart.
	log.Warn("controlplane: signing keys loaded from inline PEM (AUTHKIT_ACTIVE_KEY_ID/AUTHKIT_ACTIVE_PRIVATE_KEY_PEM) — this key is FROZEN for the process lifetime: no hot rotation and no emergency revocation without a restart. auth.keys_path is the FILE-watched, hot-rotating production path (#752).")
	return ks, nil
}

// usernames maps the site naming policy onto AuthKit's username rule.
func usernames(p config.NamingPolicy) authkit.UsernameConfig {
	u := authkit.UsernameConfig{Renames: p.Enabled, RenameInterval: p.RenameInterval}
	if u.RenameInterval == 0 {
		u.RenameInterval = -1 // AuthKit reads 0 as its default; ours is no wait.
	}
	switch p.FormerNames {
	case config.FormerNamesForever:
		u.FormerNames.Mode = authkit.FormerNamesForever
	case config.FormerNamesImmediate:
		u.FormerNames.Mode = authkit.FormerNamesImmediate
	default:
		u.FormerNames = authkit.FormerNamesConfig{Mode: authkit.FormerNamesFinite, Duration: p.FormerNameRetention}
	}
	return u
}

// authConfig is the AuthKit configuration of the control plane. Its jobs run
// on OpenRails' River fleet, in riverSchema.
func authConfig(auth *hostconfig.AuthConfig, options options, naming config.NamingPolicy, httpCfg *authkit.HTTPConfig, riverSchema string) authkit.Config {
	return authkit.Config{
		Database: authkit.DatabaseConfig{Schema: strings.TrimSpace(auth.Schema), RiverSchema: riverSchema},
		Token: authkit.TokenConfig{
			Issuer:                  strings.TrimSpace(auth.Issuer),
			IssuedAudiences:         []string{billingauth.TokenAudience},
			AllowPrivateNetworkJWKS: auth.AllowPrivateNetworkJWKS,
		},
		Keys: authkit.KeysConfig{
			Path:                  strings.TrimSpace(auth.KeysPath),
			AllowEphemeralDevKeys: auth.AllowEphemeralSigningKey,
			VerifyOnly:            auth.MintDisabled,
		},
		Frontend:     options.frontend,
		Registration: registration(options, auth),
		Username:     usernames(naming),
		APIKeys:      authkit.APIKeysConfig{Prefix: APIKeyPrefix},
		Roles:        Roles,
		HTTP:         httpCfg,
	}
}

// AuthKit is the control plane's AuthKit: the configuration and dependencies
// a standalone server builds, migrates and serves its AuthKit client from,
// over pool (OpenRails' own database, owned by the caller).
func AuthKit(cfg *config.Config, auth *hostconfig.AuthConfig, pool *pgxpool.Pool, opts ...Option) (authkit.Config, authkit.Deps, error) {
	cp, options, err := prepare(cfg, auth, pool, opts)
	if err != nil {
		return authkit.Config{}, authkit.Deps{}, err
	}
	var keySource keys.Source
	if auth.MintDisabled {
		log.Info("controlplane: auth.mint_disabled=true; running VERIFY-ONLY by declared posture (token minting disabled)")
	} else if keySource, err = inlineKeySource(auth); err != nil {
		return authkit.Config{}, authkit.Deps{}, err
	}
	httpCfg, err := clientIPPosture(cfg, auth, options)
	if err != nil {
		return authkit.Config{}, authkit.Deps{}, err
	}
	httpCfg.Groups = cp.MountedRouteGroups()
	httpCfg.APIPath = authAPIPath(auth.Issuer)
	httpCfg.RateLimits = options.rateLimitOverrides
	// The control plane serves same-origin browser clients, including auth-ui.
	// Refresh credentials stay in AuthKit's protected cookie, not browser JS.
	httpCfg.RefreshCookie = true
	deps := authkit.Deps{
		Postgres:      pool,
		KeySource:     keySource,
		Email:         options.email,
		SMS:           options.sms,
		NameAdmission: options.nameAdmission,
	}
	// AuthKit's rate limits are shared through Redis, or per process only
	// when the operator declared a single replica.
	switch {
	case options.redis != nil:
		deps.Redis = options.redis
	case !auth.AllowMemory:
		return authkit.Config{}, authkit.Deps{}, errors.New("controlplane: AuthKit rate limits need Redis (shared by replicas); set auth.allow_memory=true only for a single-process deployment")
	}
	return authConfig(auth, options, cp.naming, &httpCfg, config.RiverSchemaName(cfg)), deps, nil
}

// New is the control plane over client, the AuthKit client built from AuthKit
// with the same arguments. Closing it closes client.
func New(client *authkit.Client, cfg *config.Config, auth *hostconfig.AuthConfig, pool *pgxpool.Pool, opts ...Option) (*ControlPlane, error) {
	if client == nil {
		return nil, errors.New("controlplane: an AuthKit client is required")
	}
	cp, options, err := prepare(cfg, auth, pool, opts)
	if err != nil {
		return nil, err
	}
	cp.client = client
	cp.users = userauth.NewAuthenticator(client)
	if options.resourceServer != nil {
		if cp.resource, err = newResourceServer(*options.resourceServer, auth, options.redis); err != nil {
			return nil, err
		}
	}
	return cp, nil
}

// prepare validates the control plane's declaration: every input is required
// and a failure is a boot failure, never a silent downgrade (#469).
func prepare(cfg *config.Config, auth *hostconfig.AuthConfig, pool *pgxpool.Pool, opts []Option) (*ControlPlane, options, error) {
	options := newOptions(opts)
	if cfg == nil || auth == nil || strings.TrimSpace(auth.Issuer) == "" {
		return nil, options, errors.New("controlplane: auth.issuer is required")
	}
	if err := hostconfig.ValidateAuthTransport(auth); err != nil {
		return nil, options, err
	}
	if pool == nil {
		return nil, options, errors.New("controlplane: pgx pool is required")
	}
	if err := ValidateRegistrationMode(options.registration); err != nil {
		return nil, options, err
	}
	var pattern *regexp.Regexp
	if options.merchantCreation != nil {
		if pat := strings.TrimSpace(options.merchantCreation.SlugPattern); pat != "" {
			re, err := regexp.Compile("^(?:" + pat + ")$")
			if err != nil {
				return nil, options, fmt.Errorf("controlplane: merchant creation slug pattern: %w", err)
			}
			pattern = re
		}
	}
	namingConfig := auth.Naming
	if options.naming != nil {
		namingConfig = *options.naming
	}
	naming, err := config.NormalizeNaming(namingConfig)
	if err != nil {
		return nil, options, fmt.Errorf("controlplane: naming policy: %w", err)
	}
	return &ControlPlane{
		registration: registrationMode(options.registration), localSignIn: options.localSignIn, merchantCreation: options.merchantCreation,
		merchantCreationPattern: pattern, naming: naming,
		pool: db.WrapPool(pool, config.SchemaName(cfg)), authPrefix: authPrefix(auth.Issuer),
	}, options, nil
}

// Close closes the AuthKit client. The pool supplied to New remains owned by
// the caller.
func (c *ControlPlane) Close() {
	if c != nil && c.client != nil {
		_ = c.client.Close(context.Background())
	}
}

// Core is the control plane's AuthKit Client.
func (c *ControlPlane) Core() *authkit.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// UserAuthenticator authenticates the control plane's own user tokens in
// process (#739), for the standalone user routes and embedding hosts. Nil
// without a control plane.
func (c *ControlPlane) UserAuthenticator() billingauth.Authenticator {
	if c == nil || c.users == nil {
		return nil
	}
	return c.users
}

// Pool returns the control plane's schema-aware pgx pool over the openrails.*
// tables (#471); Pool().Raw() is the underlying pool.
func (c *ControlPlane) Pool() *db.Pool {
	if c == nil {
		return nil
	}
	return c.pool
}

// Registration is AuthKit's native self-registration mode (closed by
// default, so private OpenRails is locked down unless the host opens it).
func (c *ControlPlane) Registration() iam.RegistrationMode {
	if c == nil {
		return iam.RegistrationModeClosed
	}
	return registrationMode(c.registration)
}

// registers reports whether people can create accounts themselves (open or
// invite-only registration).
func (c *ControlPlane) registers() bool {
	return c.Registration() != iam.RegistrationModeClosed
}
