package bootstrap

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/open-rails/authkit/keys"

	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/service"
)

const (
	// DefaultBootstrapManifestPath is the conventional mounted AuthKit authority
	// bootstrap file location for standalone containers and Linux deployments.
	DefaultBootstrapManifestPath = "/etc/openrails/bootstrap.yaml"
	BootstrapManifestVersion     = 1
)

func validateMerchantManifestShape(m *BillingConfig) error {
	if m == nil {
		return fmt.Errorf("merchant manifest is required")
	}
	if m.Version != BootstrapManifestVersion {
		return fmt.Errorf("merchant bootstrap: manifest version must be %d", BootstrapManifestVersion)
	}
	for _, rawSlug := range sortedMerchantKeys(m.Merchants) {
		t := m.Merchants[rawSlug]
		slug := strings.ToLower(strings.TrimSpace(rawSlug))
		if slug == "" {
			return fmt.Errorf("merchant key is required")
		}
		if t.RemoteApplication != nil {
			if err := validateManifestRemoteApplication(slug, t.RemoteApplication); err != nil {
				return err
			}
		}
		// The configuration API's own validator: a manifest cannot declare
		// settings the API would refuse.
		if err := service.ValidateMerchantSettings(t.MerchantSettings); err != nil {
			return fmt.Errorf("merchant %q settings: %w", slug, err)
		}
		for key, account := range t.PSPs {
			normalized, err := normalizeManifestPSP(slug, key, account)
			if err != nil {
				return err
			}
			t.PSPs[key] = normalized
		}
	}
	return nil
}

// validateManifestRemoteApplication checks the per-merchant host-app
// remote_application (#527). It is registered as OWNER of the merchant's
// permission-group, so its delegated tokens administer that one merchant.
func validateManifestRemoteApplication(merchantSlug string, app *RemoteApplicationConfig) error {
	issuer := strings.TrimSpace(app.Issuer)
	if issuer == "" {
		return fmt.Errorf("merchant %q remote_application.issuer is required", merchantSlug)
	}
	if !validHTTPURL(issuer) {
		return fmt.Errorf("merchant %q remote_application.issuer must be an http or https URL", merchantSlug)
	}

	sources := 0
	if strings.TrimSpace(app.JWKSURI) != "" {
		sources++
	}
	if len(app.JWKS.Keys) > 0 {
		sources++
	}
	if len(app.PublicKeys) > 0 {
		sources++
	}
	if sources == 0 {
		return fmt.Errorf("merchant %q remote_application must set jwks_uri, jwks, or public_keys", merchantSlug)
	}
	if sources > 1 {
		return fmt.Errorf("merchant %q remote_application must set exactly one of jwks_uri, jwks, or public_keys", merchantSlug)
	}
	if jwks := strings.TrimSpace(app.JWKSURI); jwks != "" && !validHTTPURL(jwks) {
		return fmt.Errorf("merchant %q remote_application.jwks_uri must be an http or https URL", merchantSlug)
	}
	for _, jwk := range app.JWKS.Keys {
		if _, err := keys.ParsePublicJWK(jwk.authkitJWK()); err != nil {
			return fmt.Errorf("merchant %q remote_application.jwks: key %q: %w", merchantSlug, jwk.Kid, err)
		}
	}
	return nil
}

var normalizeManifestPSP = merchantbootstrap.NormalizeManifestPSP

func validHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}
