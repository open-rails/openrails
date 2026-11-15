package bootstrap

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/open-rails/authkit/keys"

	"github.com/open-rails/openrails/internal/merchantbootstrap"
)

// DefaultBootstrapManifestPath is the conventional mounted AuthKit authority
// bootstrap file location for standalone containers and Linux deployments.
const DefaultBootstrapManifestPath = "/etc/openrails/bootstrap.yaml"

// checkManifestEntry vets a manifest entry: its remote_application, then its
// declaration.
func checkManifestEntry(slug string, t MerchantConfig) error {
	if t.RemoteApplication != nil {
		if err := validateManifestRemoteApplication(slug, t.RemoteApplication); err != nil {
			return err
		}
	}
	return merchantbootstrap.CheckManifestDeclaration(slug, t.MerchantDeclaration)
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

func validHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}
