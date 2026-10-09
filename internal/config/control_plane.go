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
