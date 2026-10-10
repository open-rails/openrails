package controlplane

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/open-rails/openrails/internal/merchants"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

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
}

// ResourceScopes are the scopes of the server's resource (RFC 6749 §3.3)
// and the permissions each grants: openrails:merchant the merchant
// permissions, openrails:self none (a customer's own billing).
var ResourceScopes = map[string][]string{
	"openrails:merchant": {"merchant:*"},
	"openrails:self":     {},
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
	if auth.Resource.ID != "" {
		auth.Resource.Scopes = ResourceScopes
	}
	// A self-hosted server registers nobody unless it says so.
	reg := &auth.Registration
	if reg.NativeUserMode == "" {
		reg.NativeUserMode = iam.RegistrationModeClosed
	}
	if reg.NativeUserMode != iam.RegistrationModeClosed && reg.Verification == "" {
		reg.Verification = iam.RegistrationVerificationRequired
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
	return nil
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

// Registration is AuthKit's self-registration mode.
func (c *ControlPlane) Registration() iam.RegistrationMode {
	if c == nil || c.registration == "" {
		return iam.RegistrationModeClosed
	}
	return c.registration
}
