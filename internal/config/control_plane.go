package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/authkit/iam"
)

// ControlPlaneConfig attaches the OpenRails-owned AuthKit control plane: user
// accounts, merchant permission groups, API keys and the standalone HTTP
// surface. The standalone server and hosted products set it; hosts with their
// own authentication leave it nil and supply Deps.Authenticate instead.
type ControlPlaneConfig struct {
	// Auth is the control plane's identity configuration (issuer, keys, naming).
	Auth AuthConfig
	// Registration is AuthKit's native self-registration mode: open,
	// invite_only or closed. Empty is closed, so a self-hosted deployment
	// registers nobody; a hosted product opens it. Open and invite-only also
	// mount AuthKit's self-service API and need an email or SMS sender.
	Registration iam.RegistrationMode
	// PasswordlessLogin exposes contact-based passwordless sign-in;
	// PasswordlessAutoRegistration also creates a no-password user for a
	// verified unknown contact (requires open registration and a sender).
	PasswordlessLogin            bool
	PasswordlessAutoRegistration bool
	// FrontendBaseURL is where emailed links point. Empty uses the issuer,
	// which for a hosted product serves no pages.
	FrontendBaseURL string
	// TrustedProxies and CloudflareProxies override Config's for AuthKit's
	// client-IP resolver; only CloudflareProxies may assert CF-Connecting-IP.
	TrustedProxies    []string
	CloudflareProxies []string
	// AuthRateLimits overlays AuthKit's default rate-limit buckets by name.
	AuthRateLimits map[string]AuthRateLimit
	// MerchantCreation declares the policy for merchant names users claim.
	// Nil for operator-provisioned deployments.
	MerchantCreation *MerchantCreationConfig
	// ResourceServer accepts RFC 9068 access tokens (at+jwt) that trusted
	// issuers mint for this deployment, so a host's users reach the merchant
	// API without OpenRails holding their accounts. Nil accepts none.
	ResourceServer *ResourceServerConfig
}

// ResourceServerConfig makes the control plane an OAuth 2.0 resource server.
type ResourceServerConfig struct {
	// Identifier is this deployment's resource identifier (RFC 8707): an
	// accepted token's aud must name it.
	Identifier string
	// DPoPNonceKey (at least 32 bytes, the same on every replica) keys the
	// RFC 9449 server nonces every DPoP proof must carry.
	DPoPNonceKey string
	// TrustedIssuers are the authorization servers whose tokens are accepted.
	TrustedIssuers []TrustedIssuerConfig
}

// TrustedIssuerConfig is one authorization server and what its tokens may do.
// Its tokens carry OpenRails permissions; GroupRoles exists only for issuers
// that cannot mint them.
type TrustedIssuerConfig struct {
	// Name labels the issuer in logs.
	Name string
	// Issuer is the exact iss of its tokens.
	Issuer string
	// JWKSURI is where its signing keys are fetched; empty is
	// Issuer + /.well-known/jwks.json. Keys pins them instead.
	JWKSURI string
	Keys    []iam.RemoteApplicationKey
	// Merchants are the slugs of the merchants its tokens act for. A token
	// never names its merchant.
	Merchants []string
	// Permissions is the ceiling: a token grants only what it lies within.
	Permissions []string
	// AllowedOrigins are the browser origins (scheme://host[:port]) allowed
	// to call the merchant API cross-origin with its tokens.
	AllowedOrigins []string
	// GroupRoles maps a role the token's roles claim names to an OpenRails
	// merchant role (owner, support, viewer), for issuers that cannot mint
	// OpenRails permissions.
	GroupRoles map[string]string
}

// ValidateResourceServer refuses a resource server that would accept tokens
// it cannot bind: every issuer needs an exact iss, its merchants and a
// ceiling, and DPoP nonces need a shared key.
func ValidateResourceServer(rs *ResourceServerConfig, allowLoopback bool) error {
	if rs == nil {
		return nil
	}
	if err := validatePublicURL(strings.TrimSpace(rs.Identifier), allowLoopback, false); err != nil {
		return fmt.Errorf("resource_server.identifier: %w", err)
	}
	if len(rs.DPoPNonceKey) < 32 {
		return fmt.Errorf("resource_server.dpop_nonce_key must be at least 32 bytes")
	}
	seen := map[string]bool{}
	for i, is := range rs.TrustedIssuers {
		at := fmt.Sprintf("resource_server.trusted_issuers[%d]", i)
		issuer := strings.TrimSpace(is.Issuer)
		if err := validatePublicURL(issuer, allowLoopback, false); err != nil {
			return fmt.Errorf("%s.issuer: %w", at, err)
		}
		if seen[issuer] {
			return fmt.Errorf("%s.issuer %q is declared twice", at, issuer)
		}
		seen[issuer] = true
		if uri := strings.TrimSpace(is.JWKSURI); uri != "" {
			if len(is.Keys) > 0 {
				return fmt.Errorf("%s: jwks_uri and keys are exclusive", at)
			}
			if err := validatePublicURL(uri, allowLoopback, false); err != nil {
				return fmt.Errorf("%s.jwks_uri: %w", at, err)
			}
		}
		if len(is.Merchants) == 0 {
			return fmt.Errorf("%s.merchants is empty", at)
		}
		for _, slug := range is.Merchants {
			if strings.TrimSpace(slug) == "" {
				return fmt.Errorf("%s.merchants has an empty slug", at)
			}
		}
		if len(is.Permissions) == 0 {
			return fmt.Errorf("%s.permissions (the ceiling) is empty", at)
		}
		for _, perm := range is.Permissions {
			if !strings.HasPrefix(strings.TrimSpace(perm), "merchant:") {
				return fmt.Errorf("%s.permissions: %q is not a merchant permission", at, perm)
			}
		}
		for _, origin := range is.AllowedOrigins {
			if err := validatePublicURL(strings.TrimSpace(origin), allowLoopback, true); err != nil {
				return fmt.Errorf("%s.allowed_origins %q: %w", at, origin, err)
			}
		}
		for role, merchantRole := range is.GroupRoles {
			if strings.TrimSpace(role) == "" || strings.TrimSpace(merchantRole) == "" {
				return fmt.Errorf("%s.group_roles has an empty role", at)
			}
		}
	}
	return nil
}

// AuthRateLimit is one AuthKit rate-limit bucket.
type AuthRateLimit struct {
	Limit    int
	Window   time.Duration
	Cooldown time.Duration
}

// MerchantCreationConfig is the hosted policy for user-claimed merchant names.
// Reserved names, the pattern and the admission gate apply to
// Client.ProvisionMerchant with an owner and to renames.
type MerchantCreationConfig struct {
	// ReservedSlugs are reserved in addition to billing.ReservedMerchantSlugs.
	ReservedSlugs []string
	// ReservedEscalationRole names the root-group role whose holders may claim
	// reserved names. Empty: reserved names are never user-claimable.
	ReservedEscalationRole string
	// SlugPattern further restricts claimed names (an unanchored regexp).
	SlugPattern string
	// FreeAllowance is how many merchants a user may own before creating
	// another requires a vaulted payment method (Deps.HasVaultedPaymentMethod).
	// Zero admits every creation that passes the name policy.
	FreeAllowance int
}

// AuthConfig is the control plane's identity configuration. Remote
// applications, keys, users, groups and roles are AuthKit state, seeded
// through the control plane rather than declared here.
type AuthConfig struct {
	// The Allow* fields are narrow exceptions for local development; TestMode
	// sandbox enables none of them. AllowMemory keeps AuthKit's stores in
	// memory, AllowPrivateNetworkJWKS lets a remote application's keys be
	// fetched from a private address, AllowMissingSenders boots without email
	// or SMS senders, AllowEphemeralSigningKey generates a signing key when
	// none is configured, and AllowLoopbackHTTP admits http://localhost for
	// Issuer and RequestOrigin.
	AllowMemory              bool
	AllowPrivateNetworkJWKS  bool
	AllowMissingSenders      bool
	AllowEphemeralSigningKey bool
	AllowLoopbackHTTP        bool
	// RequestOrigin is the public origin (no path) requests arrive at, used to
	// reconstruct the URL a DPoP proof signs. It is independent of Issuer and
	// of the billing mount.
	RequestOrigin string

	// DirectPeerIP declares that clients connect directly. Behind a reverse
	// proxy, set Config.TrustedProxies instead.
	DirectPeerIP bool

	// Naming is the site naming policy for merchant names and usernames.
	Naming NamingConfig

	// Issuer is the token issuer OpenRails signs as, such as
	// "https://openrails.example.com". Required.
	Issuer string

	// ActiveKeyID and ActivePrivateKeyPEM are the inline signing key. Without
	// them the control plane reads KeysPath (or, with
	// AllowEphemeralSigningKey, generates a key).
	ActiveKeyID         string
	ActivePrivateKeyPEM string
	// PublicKeysJSON is a JSON object {kid: PEM} of further public keys
	// trusted for verification only, such as the previous active key during a
	// rotation.
	PublicKeysJSON string
	// KeysPath is the directory holding keys.json when no inline key is set.
	// Empty is AuthKit's default, /vault/auth.
	KeysPath string
	// MintDisabled declares the control plane verify-only: it mints no tokens
	// and looks for no signing key. Without it a missing signing key fails
	// construction.
	MintDisabled bool
}

// ValidateAuthTransport checks issuer trust and the independent public request origin.
// Constructors invoke this even when the host supplies Config directly.
func ValidateAuthTransport(auth *AuthConfig) error {
	if auth == nil {
		return nil
	}
	if issuer := strings.TrimSpace(auth.Issuer); issuer != "" {
		if err := validatePublicURL(issuer, auth.AllowLoopbackHTTP, false); err != nil {
			return fmt.Errorf("auth issuer: %w", err)
		}
	}
	if origin := strings.TrimSpace(auth.RequestOrigin); origin != "" {
		if err := validatePublicURL(origin, auth.AllowLoopbackHTTP, true); err != nil {
			return fmt.Errorf("auth.request_origin: %w", err)
		}
	}
	return nil
}
