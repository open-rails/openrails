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
// OpenRails merchant state. Catalog state is pushed by `openrails push-catalog`.
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
		if _, ok := seen[slug]; ok {
			return fmt.Errorf("duplicate merchant slug %q", slug)
		}
		seen[slug] = struct{}{}
	}
	return nil
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
