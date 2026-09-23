// Package config loads standalone host billing and identity configuration.
package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/open-rails/authkit"
	billing "github.com/open-rails/openrails/config"
)

// Config composes host identity with neutral billing configuration.
type Config struct {
	*billing.Config `koanf:",squash"`
	Auth            *AuthConfig `koanf:"auth,omitempty"`
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

	// Naming is normalized and validated by AuthKit once at construction.
	Naming authkit.NamingConfig `koanf:"naming,omitempty"`
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
	// explicit jwtkit.KeySource (internal/controlplane); authkit's own library
	// no longer reads any env itself. Optional: the control plane falls back to
	// KeysPath/keys.json (or, in dev, an ephemeral key) when unset.
	ActiveKeyID         string `koanf:"active_key_id,omitempty"`
	ActivePrivateKeyPEM string `koanf:"active_private_key_pem,omitempty"`
	// PublicKeysJSON is a JSON object {kid: PEM} of additional trusted public
	// keys (verify-only, e.g. a previous active key mid-rotation).
	PublicKeysJSON string `koanf:"public_keys,omitempty"`
	// KeysPath is the directory holding keys.json when no inline key material
	// is set (env AUTHKIT_KEYS_PATH; special-cased below). Empty uses
	// jwtkit.DefaultAuthKeysPath ("/vault/auth").
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

// validatePublicURL permits HTTP only for explicitly authorized loopback hosts.
func validatePublicURL(raw string, allowLoopback, originOnly bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be an absolute HTTPS URL without credentials, query or fragment")
	}
	if originOnly && u.Path != "" && u.Path != "/" {
		return fmt.Errorf("must be an origin without a path")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && allowLoopback && loopback) {
		return fmt.Errorf("must use HTTPS (HTTP requires an explicit loopback exception)")
	}
	return nil
}

// Validate checks the composed standalone host configuration.
func Validate(cfg *Config) error {
	if cfg == nil || cfg.Config == nil {
		return fmt.Errorf("standalone config is required")
	}
	if err := billing.Validate(cfg.Config); err != nil {
		return err
	}
	return cfg.Auth.ValidateTransport()
}
