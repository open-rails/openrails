// Package config loads the standalone host's billing and AuthKit configuration.
// Embedded billing receives only the separate billing Config and neutral auth.
package config

import (
	"fmt"
	"strings"

	"github.com/open-rails/authkit"
	billing "github.com/open-rails/openrails/config"
)

// Config composes standalone identity settings with the billing engine settings.
// Auth is never carried through the billing configuration object.
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
	// (e.g. "https://openrails.mysite.com"). Required outside development; in
	// development Load defaults it to api_url (or http://localhost:<port>).
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
	// discovery failure is a construction (boot) failure outside development
	// — verify-only must be a declared posture, never a silent downgrade from
	// an outage indistinguishable from intent.
	MintDisabled bool `koanf:"mint_disabled,omitempty"`
}

// Validate retains the standalone issuer policy for loaded and programmatic hosts.
func (auth *AuthConfig) Validate(development bool) error {
	if auth != nil && !development {
		if issuer := strings.TrimSpace(auth.Issuer); issuer != "" && !strings.HasPrefix(strings.ToLower(issuer), "https://") {
			return fmt.Errorf("auth issuer %q must use https outside development", issuer)
		}
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
	return cfg.Auth.Validate(cfg.IsDev())
}
