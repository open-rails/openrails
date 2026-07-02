package bootstrap

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
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
		if profileURL := strings.TrimSpace(t.Profile.LogoURL); profileURL != "" && !validHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.logo_url must be an http or https URL", slug)
		}
		if profileURL := strings.TrimSpace(t.Profile.SupportURL); profileURL != "" && !validHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.support_url must be an http or https URL", slug)
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
		for key, account := range t.RailMerchantAccounts {
			if err := validateManifestRailMerchantAccount(slug, key, account); err != nil {
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

// validInvoiceBoundaries mirrors money.NormalizeInvoiceBoundary's accepted set
// (kept inline to avoid importing the money module into bootstrap).
var validInvoiceBoundaries = map[string]struct{}{
	"calendar_month": {}, "anniversary": {}, "fixed_interval": {},
}

func validateManifestInvoice(slug string, inv *InvoiceConfig) error {
	if inv == nil {
		return nil
	}
	if inv.CollectionThreshold != nil && *inv.CollectionThreshold < 0 {
		return fmt.Errorf("merchant %q invoice.collection_threshold must be >= 0", slug)
	}
	if inv.MonthlyFloor != nil && *inv.MonthlyFloor < 0 {
		return fmt.Errorf("merchant %q invoice.monthly_floor must be >= 0", slug)
	}
	if b := strings.ToLower(strings.TrimSpace(inv.BillingPeriodBoundary)); b != "" {
		if _, ok := validInvoiceBoundaries[b]; !ok {
			return fmt.Errorf("merchant %q invoice.billing_period_boundary must be calendar_month, anniversary, or fixed_interval", slug)
		}
	}
	return nil
}

func validateManifestWastedWindows(slug string, windows []BudgetWindowConfig) error {
	seen := map[string]struct{}{}
	for i, w := range windows {
		key := strings.TrimSpace(w.Key)
		if key == "" {
			return fmt.Errorf("merchant %q delegated_invoker_wasted_spend_windows[%d].key is required", slug, i)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("merchant %q duplicate delegated_invoker_wasted_spend_windows key %q", slug, key)
		}
		seen[key] = struct{}{}
		if _, err := time.ParseDuration(strings.TrimSpace(w.Window)); err != nil {
			return fmt.Errorf("merchant %q delegated_invoker_wasted_spend_windows[%d].window %q: %w", slug, i, w.Window, err)
		}
		if w.Limit <= 0 {
			return fmt.Errorf("merchant %q delegated_invoker_wasted_spend_windows[%d].limit must be > 0", slug, i)
		}
	}
	return nil
}

func validateManifestRailMerchantAccount(slug string, key string, account RailMerchantAccountConfig) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("merchant %q accounts key is required", slug)
	}
	if len(account) != 1 {
		return fmt.Errorf("merchant %q accounts.%s must set exactly one rail block", slug, key)
	}
	for rail, cfg := range account {
		rail = strings.ToLower(strings.TrimSpace(rail))
		if rail == "" {
			return fmt.Errorf("merchant %q accounts.%s rail is required", slug, key)
		}
		// Omitted environment is valid — it follows deployment posture at
		// reconcile time (#681); explicit values must normalize.
		if strings.TrimSpace(cfg.Environment) != "" {
			if _, err := normalizeProviderEnvironment(cfg.Environment); err != nil {
				return fmt.Errorf("merchant %q accounts.%s.%s.%w", slug, key, rail, err)
			}
		}
		for secretKey := range cfg.Secrets {
			if _, err := merchants.NormalizeRailMerchantAccountSecretKey(rail, secretKey); err != nil {
				return fmt.Errorf("merchant %q accounts.%s.%s: %w", slug, key, rail, err)
			}
		}
		if rail == "solana" {
			// Solana never needs account_id — it is always derived from the signer's
			// public key, and a declared value is ignored (warned at apply). A signer
			// is required so there is a key to derive from.
			if !solanaSignerConfigured(cfg) {
				return fmt.Errorf("merchant %q accounts.%s.solana requires a signer (local_keypair private_key or vault_transit)", slug, key)
			}
			continue
		}
		if strings.TrimSpace(cfg.AccountID) == "" {
			return fmt.Errorf("merchant %q accounts.%s.%s.account_id is required (auto-discovery removed; declare account_id in the manifest)", slug, key, rail)
		}
		// #697: rail-specific format doctrine (CCBill ids are dash-joined).
		if err := config.ValidateRailAccountID(models.Rail(rail), strings.TrimSpace(cfg.AccountID)); err != nil {
			return fmt.Errorf("merchant %q accounts.%s.%s: %w", slug, key, rail, err)
		}
	}
	return nil
}

func solanaSignerConfigured(cfg ProviderRailAccountConfig) bool {
	if cfg.Signer != nil {
		return true
	}
	for k := range cfg.Secrets {
		if strings.EqualFold(strings.TrimSpace(k), "private_key") {
			return true
		}
	}
	return false
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
