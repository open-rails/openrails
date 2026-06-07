package bootstrap

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/goccy/go-yaml"

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
// infrastructure, while this file describes desired tenant/catalog state.
type BootstrapManifest struct {
	Version  int                `yaml:"version"`
	Tenants  []ManifestTenant   `yaml:"tenants,omitempty"`
	Catalogs []BootstrapCatalog `yaml:"catalogs,omitempty"`
}

// BootstrapCatalog is one top-level catalog declaration. The catalog schema is
// the existing pkg/catalog shape, embedded under catalogs[] so bootstrap can
// apply more than one catalog later without inventing a second catalog format.
type BootstrapCatalog struct {
	Name             string              `yaml:"name,omitempty"`
	DefaultCurrency  string              `yaml:"default_currency"`
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
	if len(m.Tenants) == 0 && len(m.Catalogs) == 0 {
		return fmt.Errorf("bootstrap manifest must declare at least one tenant or catalog")
	}
	if len(m.Tenants) > 0 {
		tm := m.TenantManifest()
		if err := validateTenantManifestShape(tm); err != nil {
			return err
		}
	}
	for i := range m.Catalogs {
		if _, err := m.CatalogManifest(i); err != nil {
			return err
		}
	}
	return nil
}

// TenantManifest converts the tenant section to the existing tenant manifest v2
// shape used by ReconcileTenantManifestData.
func (m *BootstrapManifest) TenantManifest() *TenantManifest {
	if m == nil {
		return nil
	}
	return &TenantManifest{Version: 2, Tenants: append([]ManifestTenant(nil), m.Tenants...)}
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
		DefaultCurrency:  c.DefaultCurrency,
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

func validateTenantManifestShape(m *TenantManifest) error {
	if m == nil {
		return fmt.Errorf("tenant manifest is required")
	}
	if m.Version != 2 {
		return fmt.Errorf("tenant bootstrap: manifest version must be 2")
	}
	seen := map[string]struct{}{}
	for i := range m.Tenants {
		t := &m.Tenants[i]
		slug := strings.ToLower(strings.TrimSpace(t.Slug))
		if slug == "" {
			return fmt.Errorf("tenant #%d slug is required", i+1)
		}
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("tenant %q name is required", slug)
		}
		if _, ok := seen[slug]; ok {
			return fmt.Errorf("duplicate tenant slug %q", slug)
		}
		seen[slug] = struct{}{}
		for _, issuer := range t.Issuers {
			if strings.TrimSpace(issuer.Issuer) == "" {
				return fmt.Errorf("tenant %q issuer is required", slug)
			}
			if !validHTTPURL(issuer.Issuer) {
				return fmt.Errorf("tenant %q issuer %q must be an http(s) URL", slug, issuer.Issuer)
			}
			if strings.TrimSpace(issuer.JWKSURI) == "" {
				return fmt.Errorf("tenant %q issuer %q jwks_uri is required", slug, issuer.Issuer)
			}
			if !validHTTPURL(issuer.JWKSURI) {
				return fmt.Errorf("tenant %q issuer %q jwks_uri must be an http(s) URL", slug, issuer.Issuer)
			}
			if len(cleanStrings(issuer.Audiences)) == 0 {
				return fmt.Errorf("tenant %q issuer %q audiences are required", slug, issuer.Issuer)
			}
		}
		for _, principal := range t.ServiceJWTPrincipals {
			if strings.TrimSpace(principal.Issuer) == "" {
				return fmt.Errorf("tenant %q service_jwt_principal issuer is required", slug)
			}
			if !validHTTPURL(principal.Issuer) {
				return fmt.Errorf("tenant %q service_jwt_principal issuer %q must be an http(s) URL", slug, principal.Issuer)
			}
			if strings.TrimSpace(principal.Subject) == "" {
				return fmt.Errorf("tenant %q service_jwt_principal subject is required", slug)
			}
			if len(cleanStrings(principal.Permissions)) == 0 {
				return fmt.Errorf("tenant %q service_jwt_principal %q permissions are required", slug, principal.Subject)
			}
			if err := validateManifestResources(slug, "service_jwt_principal "+principal.Subject, principal.Resources); err != nil {
				return err
			}
		}
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

func validateManifestResources(tenantSlug, owner string, resources []ManifestResource) error {
	for _, r := range resources {
		if strings.TrimSpace(r.Kind) == "" || strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("tenant %q %s resource kind and id are required", tenantSlug, owner)
		}
	}
	return nil
}
