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

	userauth "github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
)

// ControlPlane is OpenRails' in-process AuthKit control plane (issue #224):
// the AuthKit Client that holds identities, merchant groups and credentials,
// and the verifiers OpenRails' routes authenticate with.
//
// HARD CUT (#469): the control plane is mandatory in standalone mode — the
// standalone binary always constructs it at boot and a construction failure is
// fatal. Embedded hosts opt in with Config.ControlPlane.
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
	trustedProxies               []string
	cloudflareProxies            []string
	directPeerIP                 bool
	rateLimitOverrides           map[string]authkit.RateLimit
	redis                        *redis.Client
	merchantCreation             *MerchantCreationConfig
	resourceServer               *config.ResourceServerConfig
}

// Option configures the control plane for embedding hosts.
type Option func(*options)

// WithRegistration sets AuthKit's native self-registration mode. Empty is
// closed: standalone registers nobody; hosted products opt in through
// Config.ControlPlane.Registration.
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

// WithTrustedProxies overrides the configured reverse-proxy CIDRs of AuthKit's
// client-IP resolver.
func WithTrustedProxies(cidrs []string) Option {
	return func(o *options) { o.trustedProxies = append([]string(nil), cidrs...) }
}

// WithCloudflareProxies overrides the configured Cloudflare egress CIDRs
// (ak#298). Only these peers may assert CF-Connecting-IP.
func WithCloudflareProxies(cidrs []string) Option {
	return func(o *options) { o.cloudflareProxies = append([]string(nil), cidrs...) }
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

// clientIPPosture resolves the client-IP declaration (ak#299): host options
// override config, direct-peer excludes proxy lists, and an undeclared posture
// refuses to boot rather than sharing one rate-limit bucket behind an unknown
// proxy.
func clientIPPosture(cfg *config.Config, auth *config.AuthConfig, options options) (authkit.HTTPConfig, error) {
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
		return authkit.HTTPConfig{}, errors.New("controlplane: direct_peer_ip conflicts with trusted_proxies/cloudflare_proxies")
	case !directPeer && len(proxies) == 0 && len(cloudflare) == 0:
		return authkit.HTTPConfig{}, errors.New("controlplane: an explicit client-IP posture is required: set auth.direct_peer_ip=true, or trusted_proxies / cloudflare_proxies")
	}
	return authkit.HTTPConfig{TrustedProxies: proxies, CloudflareProxies: cloudflare, DirectPeerIP: directPeer}, nil
}

// Registration policy (#469): closed registers nobody and verifies nothing;
// open and invite-only registration require verified contacts, so they need a
// sender.
func registration(options options, auth *config.AuthConfig) authkit.RegistrationConfig {
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
func inlineKeySource(auth *config.AuthConfig) (keys.Source, error) {
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
func authConfig(auth *config.AuthConfig, options options, naming config.NamingPolicy, httpCfg *authkit.HTTPConfig, riverSchema string) authkit.Config {
	return authkit.Config{
		Database: authkit.DatabaseConfig{RiverSchema: riverSchema},
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

// New builds the OpenRails-owned AuthKit control plane from config and a pgx
// pool over the database holding AuthKit's schema (standalone: OpenRails' own
// database). The caller owns the pool.
//
// The control plane is mandatory in standalone mode (#469): every input is
// required and a failure here is a boot failure, never a silent downgrade.
func New(ctx context.Context, cfg *config.Config, auth *config.AuthConfig, pool *pgxpool.Pool, opts ...Option) (*ControlPlane, error) {
	if cfg == nil || auth == nil {
		return nil, errors.New("controlplane: auth.issuer is required (the control plane is mandatory in standalone mode, #469)")
	}
	if err := config.ValidateAuthTransport(auth); err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, errors.New("controlplane: pgx pool is required")
	}
	if strings.TrimSpace(auth.Issuer) == "" {
		return nil, errors.New("controlplane: auth.issuer is required")
	}
	options := newOptions(opts)
	if err := ValidateRegistrationMode(options.registration); err != nil {
		return nil, err
	}

	var pattern *regexp.Regexp
	if options.merchantCreation != nil {
		if pat := strings.TrimSpace(options.merchantCreation.SlugPattern); pat != "" {
			re, err := regexp.Compile("^(?:" + pat + ")$")
			if err != nil {
				return nil, fmt.Errorf("controlplane: merchant creation slug pattern: %w", err)
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
		return nil, fmt.Errorf("controlplane: naming policy: %w", err)
	}

	var keySource keys.Source
	if auth.MintDisabled {
		log.Info("controlplane: auth.mint_disabled=true; running VERIFY-ONLY by declared posture (token minting disabled)")
	} else if keySource, err = inlineKeySource(auth); err != nil {
		return nil, err
	}
	cp := &ControlPlane{
		registration: registrationMode(options.registration), localSignIn: options.localSignIn, merchantCreation: options.merchantCreation,
		merchantCreationPattern: pattern, naming: naming,
		pool: db.WrapPool(pool, config.SchemaName(cfg)), authPrefix: authPrefix(auth.Issuer),
	}
	httpCfg, err := clientIPPosture(cfg, auth, options)
	if err != nil {
		return nil, err
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
		return nil, errors.New("controlplane: AuthKit rate limits need Redis (shared by replicas); set auth.allow_memory=true only for a single-process deployment")
	}
	client, err := authkit.New(ctx, authConfig(auth, options, naming, &httpCfg, config.RiverSchemaName(cfg)), deps)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build authkit (declare auth.mint_disabled=true if verify-only is intentional, #748): %w", err)
	}
	cp.client = client
	cp.users = userauth.NewAuthenticator(client)
	if options.resourceServer != nil {
		if cp.resource, err = newResourceServer(*options.resourceServer, auth, options.redis); err != nil {
			_ = client.Close(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	return cp, nil
}

// Close releases AuthKit's resources. The host pool supplied to New remains
// owned by the caller.
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

// MerchantCreationEnabled reports whether users may create merchants: a hosted
// creation policy is declared (or#914, WithMerchantCreation).
func (c *ControlPlane) MerchantCreationEnabled() bool {
	return c != nil && c.merchantCreation != nil
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
