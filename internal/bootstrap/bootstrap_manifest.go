package bootstrap

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/merchantbootstrap"

	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
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
		if strings.TrimSpace(t.DisplayName) == "" {
			return fmt.Errorf("merchant %q display_name is required", slug)
		}
		if host := merchants.NormalizeAPIHost(t.APIHost); host != "" {
			if err := merchants.ValidateAPIHost(host); err != nil {
				return fmt.Errorf("merchant %q api_host: %w", slug, err)
			}
		}
		if profileURL := strings.TrimSpace(t.Profile.LogoURL); profileURL != "" && !validHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.logo_url must be an http or https URL", slug)
		}
		if profileURL := strings.TrimSpace(t.Profile.SupportURL); profileURL != "" && !validHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.support_url must be an http or https URL", slug)
		}
		if profileURL := strings.TrimSpace(t.Profile.SignupURL); profileURL != "" && !validHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.signup_url must be an http or https URL", slug)
		}
		if t.RemoteApplication != nil {
			if err := validateManifestRemoteApplication(slug, t.RemoteApplication); err != nil {
				return err
			}
		}
		if err := validateManifestInvoice(slug, t.Invoice); err != nil {
			return err
		}
		if err := validateManifestWastedWindows(slug, t.DelegatedInvokerWastedSpendWindows); err != nil {
			return err
		}
		// or#288: the routing policy is validated by the SAME normalizer the
		// mode-2 config API uses, so a manifest cannot declare a policy the API
		// would refuse.
		if _, err := merchantconfig.NormalizeCheckoutRouting(checkoutRoutingRules(t.CheckoutRouting)); err != nil {
			return fmt.Errorf("merchant %q %w", slug, err)
		}
		for key, account := range t.PSPs {
			if err := validateManifestPSP(slug, key, account); err != nil {
				return err
			}
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
	if _, err := remoteApplicationStaticPublicKeys(app); err != nil {
		return fmt.Errorf("merchant %q remote_application.jwks: %w", merchantSlug, err)
	}
	return nil
}

var validateManifestInvoice = merchantbootstrap.ValidateManifestInvoice
var validateManifestWastedWindows = merchantbootstrap.ValidateManifestWastedWindows
var validateManifestPSP = merchantbootstrap.ValidateManifestPSP
var solanaSignerConfigured = merchantbootstrap.SolanaSignerConfigured
var validHTTPURL = merchantbootstrap.ValidHTTPURL
