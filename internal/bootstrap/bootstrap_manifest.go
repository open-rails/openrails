package bootstrap

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
)

const (
	// DefaultBootstrapManifestPath is the conventional mounted bootstrap
	// provisioning file location for containers and Linux deployments.
	DefaultBootstrapManifestPath = "/etc/openrails/bootstrap.yaml"
	BootstrapManifestVersion     = 1
)

// BootstrapManifest is OpenRails' declarative merchant-provisioning document
// (#527 hard cut). It describes ONLY merchants. Each merchant carries its own
// host-app issuer (JWKS/public-key trust, registered as the `owner` of the
// merchant's backing org), provider accounts + secrets, and profile.
//
// There is no auth/users/global-roles section: OpenRails owns AUTHORITY, the
// host app owns IDENTITY. A host app authenticates its own users and mints
// delegated tokens; OpenRails trusts that app as the owner of one merchant and
// keeps no local accounts. Catalog state is pushed separately by
// `openrails push-merchant-catalog`.
type BootstrapManifest struct {
	Version   int                `yaml:"version"`
	Merchants []ManifestMerchant `yaml:"merchants"`
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
	if len(m.Merchants) == 0 {
		return fmt.Errorf("bootstrap manifest must declare at least one merchant")
	}
	return validateMerchantManifestShape(m.MerchantManifest())
}

// MerchantManifest converts the manifest to the internal merchant-manifest shape
// consumed by ReconcileMerchantManifestData. There is a single manifest version
// (1); this internal representation carries it through unchanged.
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
		if profileURL := strings.TrimSpace(t.Profile.LogoURL); profileURL != "" && !validHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.logo_url must be an http or https URL", slug)
		}
		if profileURL := strings.TrimSpace(t.Profile.SupportURL); profileURL != "" && !validHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.support_url must be an http or https URL", slug)
		}
		if t.Issuer != nil {
			if err := validateManifestIssuer(slug, t.Issuer); err != nil {
				return err
			}
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

// validateManifestIssuer checks the per-merchant host-app issuer (#527). The
// issuer is registered as the OWNER of the merchant's backing org, so its
// delegated tokens administer that one merchant. Exactly one trust source —
// jwks_uri (preferred, auto-rotating) XOR public_keys (static, manual) — must
// be declared.
func validateManifestIssuer(merchantSlug string, iss *ManifestIssuer) error {
	uri := strings.TrimSpace(iss.URI)
	if uri == "" {
		return fmt.Errorf("merchant %q issuer.uri is required", merchantSlug)
	}
	if !validHTTPURL(uri) {
		return fmt.Errorf("merchant %q issuer.uri must be an http or https URL", merchantSlug)
	}
	jwks := strings.TrimSpace(iss.JWKSURI)
	hasStatic := len(iss.PublicKeys) > 0
	switch {
	case jwks == "" && !hasStatic:
		return fmt.Errorf("merchant %q issuer must set jwks_uri or public_keys", merchantSlug)
	case jwks != "" && hasStatic:
		return fmt.Errorf("merchant %q issuer must set exactly one of jwks_uri or public_keys, not both", merchantSlug)
	case jwks != "" && !validHTTPURL(jwks):
		return fmt.Errorf("merchant %q issuer.jwks_uri must be an http or https URL", merchantSlug)
	}
	for _, origin := range iss.AllowedOrigins {
		if o := strings.TrimSpace(origin); o != "" && !validHTTPURL(o) {
			return fmt.Errorf("merchant %q issuer.allowed_origins entry %q must be an http or https URL", merchantSlug, origin)
		}
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
