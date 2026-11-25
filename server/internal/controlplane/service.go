package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/server/internal/adminconsole"
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
	// console is Options.Console.
	console bool
	// secondFactor: AuthKit's accounts need a second factor (TwoFactor.Mode
	// is not disabled); totp: authenticator apps are one of its methods.
	secondFactor, totp bool
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
	Log       *slog.Logger
}

// Options is what the server decides about its control plane and its
// AuthKit beside the engine's configuration.
type Options struct {
	// KeysPath is the directory of AuthKit's keys.json and totp.key; empty is
	// /vault/auth, or development keys for a loopback issuer.
	KeysPath string
	// Naming is the rename policy for merchant names.
	Naming billing.NamingConfig
	// MerchantCreation is the hosted policy for merchant names users claim
	// (or#914); nil for operator-provisioned deployments. With it, Configure
	// may open native registration.
	MerchantCreation *MerchantCreationConfig
	// Console declares the admin console's OAuth client, ConsoleClientID.
	Console bool
	// Configure adjusts the derived configuration before the product's
	// invariants are applied: a hosted product's account policy.
	Configure func(*authkit.Config)
}

// ConsoleClientID is the admin console's public OAuth client at the server's
// AuthKit, and its client id at every trusted issuer staff sign in at.
const ConsoleClientID = adminconsole.ClientID

// StaffSessionLifetime is how long a sign-in to the server's own accounts
// lasts.
const StaffSessionLifetime = 30 * 24 * time.Hour

// AccessTokenTTL is how long the access tokens of the server's resource
// last: customer tokens redeemed from a merchant backend's assertion and
// console tokens.
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
// own billing). Hosted applications declare their own additional consent scopes.
var ResourceScopes = map[string][]string{
	billing.ScopeMerchant: merchantCeiling,
	billing.ScopeSelf:     {},
}

// AuthKit is the server's AuthKit configuration, built from the engine's in
// three steps: the facts its settings already state, the rules of the
// server's own accounts (staff, by invitation, with a second factor), then,
// after opts.Configure, the product's invariants. Its jobs run on the
// engine's River fleet.
func AuthKit(cfg *billing.Config, opts Options) (authkit.Config, error) {
	if cfg == nil {
		return authkit.Config{}, errors.New("controlplane: the engine's configuration is required")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/")
	if defaultResource(base) == "" {
		return authkit.Config{}, fmt.Errorf("controlplane: public_url %q must be an https URL, or http on a loopback host: it is the server's issuer and resource", cfg.PublicURL)
	}
	var a authkit.Config
	a.Token.Issuer = base
	a.Resource.ID, a.Resource.PublicURL = base, base
	a.Frontend.BaseURL = origin(base)
	a.Keys.Path = strings.TrimSpace(opts.KeysPath)
	a.Database.Schema = config.SchemaName(cfg) + "_auth"
	// Everything is local to a loopback issuer. Elsewhere an application's
	// keys on a private network are the operator's to declare (Configure).
	a.Token.AllowPrivateNetworkJWKS = loopback(base)

	a.Registration.NativeUserMode = iam.RegistrationModeInviteOnly
	a.Registration.Verification = iam.RegistrationVerificationRequired
	a.TwoFactor.Mode = iam.TwoFactorRequired
	a.TwoFactor.Methods = []iam.TwoFactorMethod{iam.TwoFactorTOTP}
	a.Token.RefreshTokenDuration = StaffSessionLifetime
	if opts.Console {
		consoleFrontend(&a.Frontend)
	}

	if opts.Configure != nil {
		opts.Configure(&a)
	}
	if err := validate(a, opts); err != nil {
		return authkit.Config{}, err
	}
	a.Roles = Roles
	a.APIKeys.Prefix = APIKeyPrefix
	a.Token.IssuedAudiences = []string{billingauth.TokenAudience}
	a.Database.RiverSchema = config.RiverSchemaName(cfg)
	// The billing scopes keep their fixed permission ceilings beside a
	// hosted product's own; copies, so the globals never change.
	scopes := make(map[string][]string, len(a.Resource.Scopes)+len(ResourceScopes))
	for name, permissions := range a.Resource.Scopes {
		scopes[name] = slices.Clone(permissions)
	}
	for name, permissions := range ResourceScopes {
		scopes[name] = slices.Clone(permissions)
	}
	a.Resource.Scopes = scopes
	if a.AuthorizationServer.AccessTokenTTL == 0 {
		a.AuthorizationServer.AccessTokenTTL = AccessTokenTTL
	}
	if opts.Console {
		if err := declareConsoleClient(&a); err != nil {
			return authkit.Config{}, err
		}
	}
	var http authkit.HTTPConfig
	if a.HTTP != nil {
		http = *a.HTTP
	}
	// The engine's client-IP posture: trusted_proxies, and Cloudflare's
	// ranges, whose CF-Connecting-IP AuthKit takes.
	http.TrustedProxies, http.CloudflareProxies, http.DirectPeerIP = slices.Clone(cfg.TrustedProxies), slices.Clone(iputil.CloudflareRanges), false
	if http.Groups == nil {
		http.Groups = routeGroups(a.Registration.NativeUserMode, a.Resource.Enabled())
	}
	http.APIPath = authAPIPath(a.Token.Issuer)
	// Same-origin browser clients, auth-ui among them, keep refresh
	// credentials in AuthKit's protected cookie, not in script.
	http.RefreshCookie = true
	a.HTTP = &http
	return a, nil
}

// defaultResource is base when it can be the server's issuer and resource
// identifier (RFC 8707): https, or http on a loopback host; else "".
func defaultResource(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	if u.Scheme == "https" || u.Scheme == "http" && loopback(base) {
		return base
	}
	return ""
}

// loopback reports a URL on a loopback host: a local deployment.
func loopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}

// origin is raw's scheme and host.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// declareConsoleClient registers the console's client for the server's
// resource, whose openrails:merchant tokens it exchanges for.
func declareConsoleClient(auth *authkit.Config) error {
	as := &auth.AuthorizationServer
	if slices.ContainsFunc(as.Clients, func(c authkit.OAuthClientConfig) bool { return c.ID == ConsoleClientID }) {
		return fmt.Errorf("controlplane: AuthorizationServer.Clients declares %s, the admin console's client, which the server declares itself", ConsoleClientID)
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

// ConsoleClientID is the console's OAuth client the control plane declares;
// empty when the console is disabled.
func (c *ControlPlane) ConsoleClientID() string {
	if c == nil || !c.console {
		return ""
	}
	return ConsoleClientID
}

// TokenEndpoint is the token endpoint of the server's AuthKit (RFC 8414).
func (c *ControlPlane) TokenEndpoint() string {
	return strings.TrimRight(c.Issuer(), "/") + iam.OAuthTokenPath
}

// validate refuses registration the server cannot serve.
func validate(auth authkit.Config, opts Options) error {
	switch mode := auth.Registration.NativeUserMode; mode {
	case iam.RegistrationModeInviteOnly, iam.RegistrationModeClosed:
	case iam.RegistrationModeOpen:
		if opts.MerchantCreation == nil {
			return errors.New("controlplane: open self-registration needs a hosted product's MerchantCreation; the server's own accounts join by invitation")
		}
	default:
		return fmt.Errorf("controlplane: native registration mode %q is not one of open, invite_only, closed", mode)
	}
	return nil
}

// consoleFrontend points AuthKit's emailed links at the console's landings.
func consoleFrontend(front *authkit.FrontendConfig) {
	front.InvitePath, front.PasswordResetPath = "/admin/invite", "/admin/reset"
	front.VerifyPath, front.PasswordlessPath = "/admin/verify", "/admin/passwordless"
}

// New is the control plane over client, the AuthKit client built from
// AuthKit's configuration, and pool, OpenRails' database. Closing it closes
// client.
func New(logger *slog.Logger, client *authkit.Client, cfg *billing.Config, auth authkit.Config, pool *pgxpool.Pool, opts Options) (*ControlPlane, error) {
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
		console: opts.Console, secondFactor: auth.TwoFactor.Mode != iam.TwoFactorDisabled,
		totp: len(auth.TwoFactor.Methods) == 0 || slices.Contains(auth.TwoFactor.Methods, iam.TwoFactorTOTP), merchantCreation: opts.MerchantCreation, merchantCreationPattern: pattern, naming: naming,
		pool: db.WrapPool(pool, config.SchemaName(cfg)), authPrefix: authPrefix(auth.Token.Issuer),
		Log: logger,
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
