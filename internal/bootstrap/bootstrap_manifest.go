package bootstrap

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	authcore "github.com/open-rails/authkit/core"

	"github.com/open-rails/openrails/pkg/catalog"
)

const (
	// DefaultBootstrapManifestPath is the conventional mounted provisioning file
	// location for containers and Linux deployments.
	DefaultBootstrapManifestPath = "/etc/openrails/bootstrap.yaml"
	BootstrapManifestVersion     = 1
)

// BootstrapManifest is the unified declarative provisioning document. It is
// intentionally separate from config.yaml: config.yaml describes runtime
// infrastructure, while this file describes desired merchant/catalog state.
type BootstrapManifest struct {
	Version   int                         `yaml:"version"`
	Auth      *authcore.BootstrapManifest `yaml:"auth,omitempty"`
	Merchants []ManifestMerchant          `yaml:"merchants,omitempty"`
	Catalogs  []BootstrapCatalog          `yaml:"catalogs,omitempty"`
}

// BootstrapCatalog is one top-level catalog declaration. The catalog schema is
// the existing pkg/catalog shape, embedded under catalogs[] so bootstrap can
// apply more than one catalog later without inventing a second catalog format.
type BootstrapCatalog struct {
	Merchant         string              `yaml:"merchant"`
	Name             string              `yaml:"name,omitempty"`
	DefaultProviders []string            `yaml:"default_providers,omitempty"`
	TierGroups       []catalog.TierGroup `yaml:"tier_groups"`
}

// LoadBootstrapManifest reads and validates a unified bootstrap manifest.
func LoadBootstrapManifest(path string) (*BootstrapManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap manifest %s: %w", path, err)
	}
	return ParseBootstrapManifest(raw)
}

// ParseBootstrapManifest parses and validates a unified bootstrap manifest.
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
	if !m.HasAuthBootstrap() && len(m.Merchants) == 0 && len(m.Catalogs) == 0 {
		return fmt.Errorf("bootstrap manifest must declare at least one auth, merchant, or catalog section")
	}
	if len(m.Merchants) > 0 {
		tm := m.MerchantManifest()
		if err := validateMerchantManifestShape(tm); err != nil {
			return err
		}
	}
	for i := range m.Catalogs {
		if strings.TrimSpace(m.Catalogs[i].Merchant) == "" {
			return fmt.Errorf("catalog #%d merchant is required", i+1)
		}
		if _, err := m.CatalogManifest(i); err != nil {
			return err
		}
	}
	return nil
}

// HasAuthBootstrap reports whether the manifest has AuthKit-owned state to
// reconcile. A nil or empty auth section is allowed so OpenRails manifests can
// carry only merchant/catalog state.
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

// CatalogManifest converts one catalogs[] entry to the existing catalog manifest
// shape and validates it with the catalog package.
func (m *BootstrapManifest) CatalogManifest(index int) (*catalog.Manifest, error) {
	if m == nil || index < 0 || index >= len(m.Catalogs) {
		return nil, fmt.Errorf("catalog index %d out of range", index)
	}
	c := m.Catalogs[index]
	cm := &catalog.Manifest{
		Version:          catalog.SupportedVersion,
		DefaultProviders: append([]string(nil), c.DefaultProviders...),
		TierGroups:       append([]catalog.TierGroup(nil), c.TierGroups...),
	}
	if err := cm.Validate(); err != nil {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = fmt.Sprintf("#%d", index+1)
		}
		return nil, fmt.Errorf("catalog %s: %w", name, err)
	}
	return cm, nil
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
