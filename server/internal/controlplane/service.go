package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/merchants"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
)

// ControlPlane is the standalone server's multi-merchant control plane over
// its own AuthKit, which decides who every request is: merchant
// provisioning and names, the AuthKit group each merchant is, and fleet
// aggregates. Only package server builds one; the embedded engine has none.
type ControlPlane struct {
	client *authkit.Client
	// issuer is AuthKit's: the authority of its scopes.
	issuer string
	// registration is AuthKit's self-registration mode.
	registration iam.RegistrationMode
	// localSignIn mounts AuthKit's sign-in surface (Options.LocalSignIn).
	localSignIn bool
	// merchantCreation is the hosted policy for user-claimed merchant names
	// (or#914); nil otherwise.
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
	// merchants is the runtime's merchants service (BindMerchants).
	merchants *merchants.Service
}

// Options is what the server decides about its control plane beside
// AuthKit's own configuration.
type Options struct {
	// LocalSignIn serves sign-in to the server's own accounts; off, people
	// arrive with trusted issuers' access tokens and AuthKit serves only its
	// JWKS.
	LocalSignIn bool
	// Naming is the rename policy for merchant names.
	Naming config.NamingConfig
	// MerchantCreation is the hosted policy for merchant names users claim
	// (or#914); nil for operator-provisioned deployments.
	MerchantCreation *MerchantCreationConfig
	// Console: the server's own accounts act on a merchant through a token
	// exchanged for it (ConsoleClientID), as its admin console does; off
	// when the console signs staff in at a trusted issuer.
	Console bool
}

// ConsoleClientID is the admin console's first-party OAuth client, which the
// server declares: a public client trading the person's session token for one
// bound to the merchant they act on (RFC 8693 token exchange naming the
// merchant's group).
const ConsoleClientID = "openrails-console"

// AccessTokenTTL is how long the access tokens of the server's resource last
// unless auth.authorization_server.access_token_ttl says otherwise: customer
// tokens redeemed from a merchant backend's assertion and console tokens.
const AccessTokenTTL = 15 * time.Minute

// merchantCeiling is what a token granted openrails:merchant may hold: every
// merchant permission in its merchant's group and, for a person, OpenRails'
// root: permissions in the root group.
var merchantCeiling = func() []string {
	out := []string{"merchant:*"}
	for _, p := range billing.Permissions() {
		if p.Persona() == "root" {
			out = append(out, string(p))
		}
	}
	return out
}()

// ResourceScopes are the scopes of the server's resource (RFC 6749 §3.3)
// and the permissions each grants: openrails:merchant the merchant
// permissions and OpenRails' root: ones, openrails:self none (a customer's
// own billing), openrails:link none (consent to link the merchant's own
// customer to the account).
var ResourceScopes = map[string][]string{
	billing.ScopeMerchant: merchantCeiling,
	billing.ScopeSelf:     {},
	"openrails:link":      {},
}

// AuthKit is the server's AuthKit configuration: the host's auth, passed
// through, with the product's own settings over it. Its jobs run on
// OpenRails' River fleet.
func AuthKit(cfg *config.Config, auth authkit.Config, opts Options) (authkit.Config, error) {
	if cfg == nil || strings.TrimSpace(auth.Token.Issuer) == "" {
		return authkit.Config{}, errors.New("controlplane: auth.token.issuer is required")
	}
	if err := validate(auth, opts); err != nil {
		return authkit.Config{}, err
	}
	auth.Roles = Roles
	auth.APIKeys.Prefix = APIKeyPrefix
	if len(auth.Token.IssuedAudiences) == 0 {
		auth.Token.IssuedAudiences = []string{billingauth.TokenAudience}
	}
	auth.Database.RiverSchema = config.RiverSchemaName(cfg)
	if auth.Resource.ID == "" {
		auth.Resource.ID = defaultResource(cfg.PublicBillingBaseURL)
	}
	if auth.Resource.ID != "" {
		auth.Resource.Scopes = ResourceScopes
		if auth.AuthorizationServer.AccessTokenTTL == 0 {
			auth.AuthorizationServer.AccessTokenTTL = AccessTokenTTL
		}
		if opts.Console {
			if err := declareConsoleClient(&auth); err != nil {
				return authkit.Config{}, err
			}
		}
	}
	// Local staff join by invitation; issuer-only servers enroll nobody.
	// An explicitly closed policy remains more restrictive.
	reg := &auth.Registration
	if reg.NativeUserMode == "" {
		reg.NativeUserMode = iam.RegistrationModeClosed
		if opts.LocalSignIn {
			reg.NativeUserMode = iam.RegistrationModeInviteOnly
		}
	}
	if reg.NativeUserMode != iam.RegistrationModeClosed && reg.Verification == "" {
		reg.Verification = iam.RegistrationVerificationRequired
	}
	if opts.LocalSignIn {
		consoleFrontend(&auth.Frontend, cfg.PublicBillingBaseURL, auth.Token.Issuer)
	}
	var http authkit.HTTPConfig
	if auth.HTTP != nil {
		http = *auth.HTTP
	}
	if !http.DirectPeerIP && len(http.TrustedProxies) == 0 && len(http.CloudflareProxies) == 0 {
		http.TrustedProxies, http.CloudflareProxies = cfg.TrustedProxies, cfg.CloudflareProxies
	}
	if http.Groups == nil {
		http.Groups = routeGroups(reg.NativeUserMode, auth.Resource.Enabled())
	}
	http.APIPath = authAPIPath(auth.Token.Issuer)
	// Same-origin browser clients, auth-ui among them, keep refresh
	// credentials in AuthKit's protected cookie, not in script.
	http.RefreshCookie = true
	auth.HTTP = &http
	return auth, nil
}

// defaultResource is the server's resource identifier (RFC 8707) when
// auth.resource.id names none: its public API base, where browsers and PSPs
// reach it, when that is https (http on a loopback host); else none.
func defaultResource(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	host := u.Hostname()
	if u.Scheme == "https" || u.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback()) {
		return base
	}
	return ""
}

// declareConsoleClient registers ConsoleClientID for the server's resource,
// whose openrails:merchant tokens it exchanges for.
func declareConsoleClient(auth *authkit.Config) error {
	as := &auth.AuthorizationServer
	if slices.ContainsFunc(as.Clients, func(c authkit.OAuthClientConfig) bool { return c.ID == ConsoleClientID }) {
		return fmt.Errorf("controlplane: auth.authorization_server.clients declares %s, the admin console's client, which the server declares itself", ConsoleClientID)
	}
	resource := auth.Resource.ID
	as.Clients = append(as.Clients, authkit.OAuthClientConfig{ID: ConsoleClientID, Name: "Admin console",
		GrantTypes: []authkit.OAuthGrantType{authkit.GrantTokenExchange}, Resources: []string{resource}})
	i := slices.IndexFunc(as.Resources, func(r authkit.ResourceServerConfig) bool { return r.ID == resource })
	if i < 0 {
		as.Resources = append(as.Resources, authkit.ResourceServerConfig{ID: resource})
		i = len(as.Resources) - 1
	}
	r := &as.Resources[i]
	if !slices.Contains(r.Scopes, billing.ScopeMerchant) {
		r.Scopes = append(r.Scopes, billing.ScopeMerchant)
	}
	for _, p := range merchantCeiling {
		if !slices.Contains(r.Permissions, p) {
			r.Permissions = append(r.Permissions, p)
		}
	}
	return nil
}

// TokenEndpoint is the token endpoint of the server's AuthKit (RFC 8414).
func (c *ControlPlane) TokenEndpoint() string {
	return strings.TrimRight(c.Issuer(), "/") + iam.OAuthTokenPath
}

// validate refuses registration the server cannot serve.
func validate(auth authkit.Config, opts Options) error {
	mode := auth.Registration.NativeUserMode
	switch mode {
	case "", iam.RegistrationModeOpen, iam.RegistrationModeInviteOnly, iam.RegistrationModeClosed:
	default:
		return fmt.Errorf("controlplane: auth.registration.native_user_mode %q is not one of open, invite_only, closed", mode)
	}
	registers := mode == iam.RegistrationModeOpen || mode == iam.RegistrationModeInviteOnly
	if (registers || auth.Registration.PasswordlessLogin) && !opts.LocalSignIn {
		return errors.New("controlplane: registration and passwordless login need local sign-in")
	}
	if mode == iam.RegistrationModeOpen {
		return errors.New("controlplane: open self-registration is unavailable on the standalone server; use auth.registration.native_user_mode=invite_only (the local sign-in default), or closed")
	}
	return nil
}

// Console link landings belong to /admin, not the issuer's JSON API path.
// Explicit frontend settings stay with the operator who configured them.
func consoleFrontend(front *authkit.FrontendConfig, publicURL, issuer string) {
	if front.BaseURL == "" {
		base := publicURL
		if base == "" {
			base = issuer
		}
		if parsed, err := url.Parse(base); err == nil && parsed.Host != "" {
			parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", "", ""
			front.BaseURL = strings.TrimRight(parsed.String(), "/")
		}
	}
	for target, path := range map[*string]string{
		&front.InvitePath: "/admin/invite", &front.PasswordResetPath: "/admin/reset",
		&front.VerifyPath: "/admin/verify", &front.PasswordlessPath: "/admin/passwordless",
	} {
		if *target == "" {
			*target = path
		}
	}
}

// New is the control plane over client, the AuthKit client built from
// AuthKit's configuration, and pool, OpenRails' database. Closing it closes
// client.
func New(client *authkit.Client, cfg *config.Config, auth authkit.Config, pool *pgxpool.Pool, opts Options) (*ControlPlane, error) {
	switch {
	case client == nil:
		return nil, errors.New("controlplane: an AuthKit client is required")
	case cfg == nil:
		return nil, errors.New("controlplane: the engine's configuration is required")
	case pool == nil:
		return nil, errors.New("controlplane: pgx pool is required")
	}
	var pattern *regexp.Regexp
	if opts.MerchantCreation != nil {
		if pat := strings.TrimSpace(opts.MerchantCreation.SlugPattern); pat != "" {
			re, err := regexp.Compile("^(?:" + pat + ")$")
			if err != nil {
				return nil, fmt.Errorf("controlplane: merchant creation slug pattern: %w", err)
			}
			pattern = re
		}
	}
	naming, err := config.NormalizeNaming(opts.Naming)
	if err != nil {
		return nil, fmt.Errorf("controlplane: naming policy: %w", err)
	}
	return &ControlPlane{
		client: client, issuer: strings.TrimSpace(auth.Token.Issuer), registration: auth.Registration.NativeUserMode,
		localSignIn: opts.LocalSignIn, merchantCreation: opts.MerchantCreation, merchantCreationPattern: pattern, naming: naming,
		pool: db.WrapPool(pool, config.SchemaName(cfg)), authPrefix: authPrefix(auth.Token.Issuer),
	}, nil
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

// Pool returns the control plane's schema-aware pgx pool over the openrails.*
// tables (#471); Pool().Raw() is the underlying pool.
func (c *ControlPlane) Pool() *db.Pool {
	if c == nil {
		return nil
	}
	return c.pool
}

// Issuer is AuthKit's issuer: the authority of the server's own accounts.
func (c *ControlPlane) Issuer() string {
	if c == nil {
		return ""
	}
	return c.issuer
}

// Registration is AuthKit's self-registration mode.
func (c *ControlPlane) Registration() iam.RegistrationMode {
	if c == nil || c.registration == "" {
		return iam.RegistrationModeClosed
	}
	return c.registration
}
