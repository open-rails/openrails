package hostconfig

// The standalone server's own configuration: what it adds to the engine's
// (internal/config), shared by the packages it composes. The embedded engine
// links none of it.

import (
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/authkit/iam"

	billing "github.com/open-rails/openrails/internal/config"
)

// ResourceServerConfig makes the standalone server an OAuth 2.0 resource
// server.
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
	// to call the admin API cross-origin with its tokens.
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
	if err := billing.ValidatePublicURL(strings.TrimSpace(rs.Identifier), allowLoopback, false); err != nil {
		return fmt.Errorf("resource_server.identifier: %w", err)
	}
	if len(rs.DPoPNonceKey) < 32 {
		return fmt.Errorf("resource_server.dpop_nonce_key must be at least 32 bytes")
	}
	seen := map[string]bool{}
	for i, is := range rs.TrustedIssuers {
		at := fmt.Sprintf("resource_server.trusted_issuers[%d]", i)
		issuer := strings.TrimSpace(is.Issuer)
		if err := billing.ValidatePublicURL(issuer, allowLoopback, false); err != nil {
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
			if err := billing.ValidatePublicURL(uri, allowLoopback, false); err != nil {
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
			if err := billing.ValidatePublicURL(strings.TrimSpace(origin), allowLoopback, true); err != nil {
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
// Server.ProvisionMerchant with an owner and to renames.
type MerchantCreationConfig struct {
	// ReservedSlugs are reserved in addition to billing.ReservedMerchantSlugs.
	ReservedSlugs []string
	// ReservedEscalationRole names the root-group role whose holders may claim
	// reserved names. Empty: reserved names are never user-claimable.
	ReservedEscalationRole string
	// SlugPattern further restricts claimed names (an unanchored regexp).
	SlugPattern string
	// FreeAllowance is how many merchants a user may own before creating
	// another requires a vaulted payment method (server.Deps.HasVaultedPaymentMethod).
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
	// proxy, set the engine's TrustedProxies instead.
	DirectPeerIP bool

	// Naming is the site naming policy for merchant names and usernames.
	Naming billing.NamingConfig

	// Schema is the Postgres schema of the server's AuthKit tables; empty is
	// AuthKit's default, profiles. Deployments sharing a database without
	// sharing accounts use different schemas.
	Schema string

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
		if err := billing.ValidatePublicURL(issuer, auth.AllowLoopbackHTTP, false); err != nil {
			return fmt.Errorf("auth issuer: %w", err)
		}
	}
	if origin := strings.TrimSpace(auth.RequestOrigin); origin != "" {
		if err := billing.ValidatePublicURL(origin, auth.AllowLoopbackHTTP, true); err != nil {
			return fmt.Errorf("auth.request_origin: %w", err)
		}
	}
	return nil
}

// ConsoleIssuer is the authorization server the console signs staff in at,
// as an OAuth 2.0 public client (code flow with PKCE and DPoP).
type ConsoleIssuer struct {
	// URL is the issuer: one of the resource server's trusted issuers.
	URL string
	// ClientID is the console's public client there, registered with the
	// console path plus /callback as a redirect URI.
	ClientID string
	// Name is shown on the sign-in button; empty is the trusted issuer's.
	Name string
	// Scope is what the console asks for; empty is ConsoleScope. An issuer
	// that grants refresh tokens only for offline_access needs it added.
	Scope string
}

// ConsoleScope is what the console asks a trusted issuer for by default.
const ConsoleScope = "openid profile email openrails:merchant"

// ResolveConsoleIssuer checks console against the resource server: its URL
// must be a trusted issuer's, and it needs a client id.
func ResolveConsoleIssuer(console *ConsoleIssuer, rs *ResourceServerConfig) (issuer, name, resource string, err error) {
	if rs == nil {
		return "", "", "", fmt.Errorf("admin console issuer: declare resource_server, which trusts it")
	}
	url := strings.TrimRight(strings.TrimSpace(console.URL), "/")
	for _, is := range rs.TrustedIssuers {
		if strings.TrimRight(strings.TrimSpace(is.Issuer), "/") != url {
			continue
		}
		if strings.TrimSpace(console.ClientID) == "" {
			return "", "", "", fmt.Errorf("admin console issuer %q: client_id is required", url)
		}
		name = strings.TrimSpace(console.Name)
		if name == "" {
			name = strings.TrimSpace(is.Name)
		}
		if name == "" {
			name = url
		}
		return strings.TrimSpace(is.Issuer), name, strings.TrimSpace(rs.Identifier), nil
	}
	return "", "", "", fmt.Errorf("admin console issuer %q is not one of resource_server.trusted_issuers", url)
}
