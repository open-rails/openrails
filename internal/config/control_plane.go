package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/merchant"
)

// ControlPlaneConfig attaches the OpenRails-owned AuthKit control plane: user
// accounts, merchant permission groups, API keys and the standalone HTTP
// surface. The standalone server and hosted products set it; hosts with their
// own authentication leave it nil and supply Deps.Authenticate instead.
type ControlPlaneConfig struct {
	// Auth is the control plane's identity configuration (issuer, keys, naming).
	Auth AuthConfig
	// HostedPosture opens AuthKit registration and mounts the full AuthKit API.
	HostedPosture bool
	// PasswordlessLogin exposes contact-based passwordless sign-in;
	// PasswordlessAutoRegistration also creates a no-password user for a
	// verified unknown contact (requires HostedPosture and a sender).
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
}

// AuthRateLimit is one AuthKit rate-limit bucket.
type AuthRateLimit struct {
	Limit    int
	Window   time.Duration
	Cooldown time.Duration
}

// MerchantCreationConfig is the hosted policy for user-claimed merchant names
// (or#914). Reserved names, the pattern and the admission gate apply to
// ProvisionMerchant with an owner and to renames.
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

// AuthConfig holds OpenRails' AuthKit control-plane configuration. Standalone
// OpenRails authenticates users/admins through this control plane; remote
// applications, JWKS/public keys, API keys, users, permission groups, roles, and
// permissions are seeded as AuthKit state rather than trusted from runtime
// config.yaml/.env issuer allow-lists.
type AuthConfig struct {
	// Narrow local-host exceptions; payment sandbox posture never enables these.
	AllowMemory              bool `koanf:"allow_memory,omitempty"`
	AllowPrivateNetworkJWKS  bool `koanf:"allow_private_network_jwks,omitempty"`
	AllowMissingSenders      bool `koanf:"allow_missing_senders,omitempty"`
	AllowEphemeralSigningKey bool `koanf:"allow_ephemeral_signing_key,omitempty"`
	AllowLoopbackHTTP        bool `koanf:"allow_loopback_http,omitempty"`
	// RequestOrigin is the trusted public origin for DPoP request reconstruction.
	// It has no path and is independent of the token issuer and billing mount.
	RequestOrigin string `koanf:"request_origin,omitempty"`

	// DirectPeerIP declares that AuthKit receives the client connection directly.
	// Configure trusted_proxies instead when a reverse proxy is in front.
	DirectPeerIP bool `koanf:"direct_peer_ip,omitempty"`

	// Naming is the site naming policy for merchant names and usernames.
	Naming merchant.NamingConfig `koanf:"naming,omitempty"`
	// HARDCUT (#312/#537): there is no `auth.operator_tenant_slug` /
	// `auth.operator_tenant_admin_roles`. Admin authority is live merchant-local
	// AuthKit merchant permission-group state (or a deployment-minted admin API
	// key) - deployment authority, not membership in a separate operator group.
	// Load rejects the
	// deprecated keys.

	// Issuer is the AuthKit token issuer OpenRails signs as
	// (e.g. "https://openrails.mysite.com"). Standalone authentication requires
	// it explicitly; no URL setting supplies an issuer fallback.
	Issuer string `koanf:"issuer,omitempty"`

	// Inline JWT signing-key material (env AUTHKIT_ACTIVE_KEY_ID /
	// AUTHKIT_ACTIVE_PRIVATE_KEY_PEM / AUTHKIT_PUBLIC_KEYS, the canonical
	// AuthKit binary names since ak#266/v0.89.0 — special-cased in
	// envKeyToConfigKey so they land here instead of a mechanical auth.* split).
	// Read ONCE here at the config-load boundary and handed to authkit as an
	// explicit key source (internal/controlplane); AuthKit reads no
	// environment itself. Optional: the control plane falls back to
	// KeysPath/keys.json (or, in dev, an ephemeral key) when unset.
	ActiveKeyID         string `koanf:"active_key_id,omitempty"`
	ActivePrivateKeyPEM string `koanf:"active_private_key_pem,omitempty"`
	// PublicKeysJSON is a JSON object {kid: PEM} of additional trusted public
	// keys (verify-only, e.g. a previous active key mid-rotation).
	PublicKeysJSON string `koanf:"public_keys,omitempty"`
	// KeysPath is the directory holding keys.json when no inline key material
	// is set (env AUTHKIT_KEYS_PATH; special-cased below). Empty uses AuthKit's
	// default, /vault/auth.
	KeysPath string `koanf:"keys_path,omitempty"`
	// MintDisabled DECLARES the control plane verify-only (#748): token
	// minting is intentionally off, so internal/controlplane.New never even
	// attempts signing-key discovery. Without this flag, a signing-key
	// discovery failure is a construction (boot) failure
	// — verify-only must be a declared posture, never a silent downgrade from
	// an outage indistinguishable from intent.
	MintDisabled bool `koanf:"mint_disabled,omitempty"`
}

// ValidateTransport checks issuer trust and the independent public request origin.
// Constructors invoke this even when the host supplies Config directly.
func (auth *AuthConfig) ValidateTransport() error {
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
