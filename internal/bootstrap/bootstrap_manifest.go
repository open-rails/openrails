package bootstrap

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
)

const (
	// DefaultBootstrapManifestPath is the conventional mounted bootstrap
	// provisioning file location for containers and Linux deployments.
	DefaultBootstrapManifestPath = "/etc/openrails/bootstrap.yaml"
	BootstrapManifestVersion     = 1
)

// BootstrapManifest is the OpenRails control-plane authority document (#531).
// It is intentionally not a merchant config or catalog document: merchants,
// provider credentials, and products/prices live in their own files.
type BootstrapManifest struct {
	Version   int                        `yaml:"version"`
	Authority BootstrapAuthorityManifest `yaml:"authority"`
}

type BootstrapAuthorityManifest struct {
	// BootstrapMerchantSlug names the merchant/permission-group whose OpenRails admin role
	// should be seeded. OpenRails does not have a global admin org in this repo;
	// admin authority is merchant-scoped, so the merchant must already exist.
	BootstrapMerchantSlug   string `yaml:"bootstrap_merchant_slug"`
	InitialAdminUserID string `yaml:"initial_admin_user_id,omitempty"`
	// MintInitialAPIKey defaults true when omitted. Set false when the
	// deploy will create admin access through another AuthKit path.
	MintInitialAPIKey *bool `yaml:"mint_initial_api_key,omitempty"`
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
	if strings.TrimSpace(m.Authority.BootstrapMerchantSlug) == "" {
		return fmt.Errorf("bootstrap manifest authority.bootstrap_merchant_slug is required")
	}
	return nil
}

func (m *BootstrapManifest) BootstrapOptions() controlplane.BootstrapOptions {
	if m == nil {
		return controlplane.BootstrapOptions{}
	}
	mintInitialAPIKey := true
	if m.Authority.MintInitialAPIKey != nil {
		mintInitialAPIKey = *m.Authority.MintInitialAPIKey
	}
	return controlplane.BootstrapOptions{
		BootstrapMerchantSlug:   strings.ToLower(strings.TrimSpace(m.Authority.BootstrapMerchantSlug)),
		InitialAdminUserID: strings.TrimSpace(m.Authority.InitialAdminUserID),
		MintInitialAPIKey:  mintInitialAPIKey,
	}
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
		if strings.TrimSpace(t.DisplayName) == "" {
			return fmt.Errorf("merchant %q display_name is required", slug)
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
// issuer is registered as the OWNER of the merchant's permission-group, so its
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
	if _, err := normalizeProviderEnvironment(account.Environment); err != nil {
		return fmt.Errorf("merchant %q provider_accounts[%d].%w", slug, idx, err)
	}
	if _, _, err := manifestProviderAccountMode(account.Mode); err != nil {
		return fmt.Errorf("merchant %q provider_accounts[%d].%w", slug, idx, err)
	}
	for key, source := range account.Secrets {
		if _, err := merchants.NormalizeProviderAccountSecretKey(providerType, key); err != nil {
			return fmt.Errorf("merchant %q provider_accounts[%d]: %w", slug, idx, err)
		}
		if source.RefCount() != 1 {
			return fmt.Errorf("merchant %q provider_accounts[%d].secrets.%s must set exactly one of value, env, file, vault", slug, idx, key)
		}
	}
	if strings.TrimSpace(account.AccountID) == "" && !manifestProviderAccountHasDiscoverableIdentity(providerType, account.Secrets) {
		return fmt.Errorf("merchant %q provider_accounts[%d].account_id is required (auto-discovery removed; declare account_id, or for ccbill provide secrets.account_config)", slug, idx)
	}
	return nil
}

// manifestProviderAccountHasDiscoverableIdentity reports whether the account_id
// can be derived without a declared value. Live-credential auto-discovery
// (stripe/nmi whoami) was removed (#592); only CCBill's account_id is derivable
// from DECLARATIVE config (account_config).
func manifestProviderAccountHasDiscoverableIdentity(providerType string, secrets map[string]ManifestSecretSource) bool {
	if normalizeManifestProviderType(providerType) != config.RailTypeCCBill {
		return false
	}
	values, err := newManifestSecretValues(config.RailTypeCCBill, secrets)
	if err != nil {
		return false
	}
	_, ok := values.sources["account_config"]
	return ok
}

const (
	configRailRolePrimary   = "primary"
	configRailRoleSecondary = "secondary"
	configRailRoleLegacy    = "legacy"
)

func validHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
