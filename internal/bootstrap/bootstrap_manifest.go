package bootstrap

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	authcore "github.com/open-rails/authkit/core"
)

const (
	// DefaultBootstrapManifestPath is the conventional mounted bootstrap
	// provisioning file location for containers and Linux deployments. It
	// contains auth + merchant definitions only; catalog state lives in the
	// separate catalog manifest.
	DefaultBootstrapManifestPath = "/etc/openrails/bootstrap.yaml"
	BootstrapManifestVersion     = 1
)

// BootstrapManifest is the auth/merchant declarative provisioning document. It
// is intentionally separate from config.yaml: config.yaml describes runtime
// infrastructure, while this file describes desired AuthKit authority and
// OpenRails merchant state. Catalog state is pushed by
// `openrails push-merchant-catalog`.
type BootstrapManifest struct {
	Version   int                         `yaml:"version"`
	Auth      *authcore.BootstrapManifest `yaml:"auth,omitempty"`
	Merchants []ManifestMerchant          `yaml:"merchants,omitempty"`
}

// LoadBootstrapManifest reads and validates a bootstrap manifest.
func LoadBootstrapManifest(path string) (*BootstrapManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap manifest %s: %w", path, err)
	}
	return ParseBootstrapManifest(raw)
}

// ParseBootstrapManifest parses and validates a bootstrap manifest.
func ParseBootstrapManifest(raw []byte) (*BootstrapManifest, error) {
	var manifest BootstrapManifest
	if err := yaml.UnmarshalWithOptions(raw, &manifest, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parse bootstrap manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// Validate normalizes and validates the manifest in place.
func (m *BootstrapManifest) Validate() error {
	if m == nil {
		return fmt.Errorf("bootstrap manifest is required")
	}
	if m.Version != BootstrapManifestVersion {
		return fmt.Errorf("bootstrap manifest version must be %d", BootstrapManifestVersion)
	}
	if !m.HasAuthBootstrap() && len(m.Merchants) == 0 {
		return fmt.Errorf("merchant manifest must declare at least one auth or merchant section")
	}
	if len(m.Merchants) > 0 {
		tm := m.MerchantManifest()
		if err := validateMerchantManifestShape(tm); err != nil {
			return err
		}
	}
	return nil
}

// HasAuthBootstrap reports whether the manifest has AuthKit-owned state to
// reconcile. A nil or empty auth section is allowed so OpenRails manifests can
// carry only merchant state.
func (m *BootstrapManifest) HasAuthBootstrap() bool {
	if m == nil || m.Auth == nil {
		return false
	}
	return len(m.Auth.Users) > 0 || len(m.Auth.GlobalRoles) > 0 || len(m.Auth.Orgs) > 0
}

// MerchantManifest converts the merchant section to the internal merchant-manifest
// shape consumed by ReconcileMerchantManifestData. There is a single manifest
// version (1); this internal representation carries it through unchanged.
func (m *BootstrapManifest) MerchantManifest() *MerchantManifest {
	if m == nil {
		return nil
	}
	return &MerchantManifest{Version: BootstrapManifestVersion, Merchants: append([]ManifestMerchant(nil), m.Merchants...)}
}

func validateMerchantManifestShape(m *MerchantManifest) error {
	if m == nil {
		return fmt.Errorf("merchant manifest is required")
	}
	if m.Version != BootstrapManifestVersion {
		return fmt.Errorf("merchant bootstrap: manifest version must be %d", BootstrapManifestVersion)
	}
	seen := map[string]struct{}{}
	for i := range m.Merchants {
		t := &m.Merchants[i]
		slug := strings.ToLower(strings.TrimSpace(t.Slug))
		if slug == "" {
			return fmt.Errorf("merchant #%d slug is required", i+1)
		}
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("merchant %q name is required", slug)
		}
		if len(t.Issuers) > 0 {
			return fmt.Errorf("merchant %q issuers is removed; declare JWKS/public-key trust in top-level auth.orgs[].issuers (AuthKit remote applications)", slug)
		}
		if profileURL := strings.TrimSpace(t.Profile.LogoURL); profileURL != "" && !validHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.logo_url must be an http or https URL", slug)
		}
		if profileURL := strings.TrimSpace(t.Profile.SupportURL); profileURL != "" && !validHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.support_url must be an http or https URL", slug)
		}
		for j := range t.ProviderAccounts {
			if err := validateManifestProviderAccount(slug, j, t.ProviderAccounts[j]); err != nil {
				return err
			}
		}
		if _, ok := seen[slug]; ok {
			return fmt.Errorf("duplicate merchant slug %q", slug)
		}
		seen[slug] = struct{}{}
	}
	return nil
}

func validateManifestProviderAccount(slug string, idx int, account ManifestProviderAccount) error {
	providerType := strings.ToLower(strings.TrimSpace(account.ProviderType))
	if providerType == "" {
		return fmt.Errorf("merchant %q provider_accounts[%d].provider_type is required", slug, idx)
	}
	if role := strings.ToLower(strings.TrimSpace(account.Role)); role != "" {
		switch role {
		case configProcessorRolePrimary, configProcessorRoleSecondary, configProcessorRoleLegacy:
		default:
			return fmt.Errorf("merchant %q provider_accounts[%d].role must be primary, secondary, or legacy", slug, idx)
		}
	}
	if status := strings.ToLower(strings.TrimSpace(account.Status)); status != "" {
		switch status {
		case "enabled", "disabled":
		default:
			return fmt.Errorf("merchant %q provider_accounts[%d].status must be enabled or disabled", slug, idx)
		}
	}
	if strings.TrimSpace(account.AccountID) == "" && len(account.Secrets) == 0 {
		return fmt.Errorf("merchant %q provider_accounts[%d] must declare account_id, secrets, or both", slug, idx)
	}
	for key, source := range account.Secrets {
		if _, err := merchantSecretName(providerType, key); err != nil {
			return fmt.Errorf("merchant %q provider_accounts[%d]: %w", slug, idx, err)
		}
		if source.RefCount() != 1 {
			return fmt.Errorf("merchant %q provider_accounts[%d].secrets.%s must set exactly one of value, env, file, vault", slug, idx, key)
		}
	}
	return nil
}

const (
	configProcessorRolePrimary   = "primary"
	configProcessorRoleSecondary = "secondary"
	configProcessorRoleLegacy    = "legacy"
)

func validHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
