package bootstrap

import (
	"fmt"
	"net/url"
	"strings"
	"time"

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
		if t.Issuer != nil {
			if err := validateManifestIssuer(slug, t.Issuer); err != nil {
				return err
			}
		}
		if err := validateManifestInvoice(slug, t.Invoice); err != nil {
			return err
		}
		if err := validateManifestWastedWindows(slug, t.DelegatedInvokerWastedSpendWindows); err != nil {
			return err
		}
		for key, account := range t.ProviderAccounts {
			if err := validateManifestProviderAccount(slug, key, account); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateManifestIssuer checks the per-merchant host-app issuer (#527). The
// issuer is registered as the OWNER of the merchant's permission-group, so its
// delegated tokens administer that one merchant. Exactly one trust source —
// jwks_uri (preferred, auto-rotating) XOR public_keys (static, manual) — must
// be declared.
func validateManifestIssuer(merchantSlug string, iss *IssuerConfig) error {
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

func validateManifestProviderAccount(slug string, key string, account ProviderAccountConfig) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("merchant %q provider_accounts key is required", slug)
	}
	if len(account) != 1 {
		return fmt.Errorf("merchant %q provider_accounts.%s must set exactly one rail block", slug, key)
	}
	for rail, cfg := range account {
		rail = strings.ToLower(strings.TrimSpace(rail))
		if rail == "" {
			return fmt.Errorf("merchant %q provider_accounts.%s rail is required", slug, key)
		}
		if _, err := normalizeProviderEnvironment(cfg.Environment); err != nil {
			return fmt.Errorf("merchant %q provider_accounts.%s.%s.%w", slug, key, rail, err)
		}
		for secretKey := range cfg.Secrets {
			if _, err := merchants.NormalizeProviderAccountSecretKey(rail, secretKey); err != nil {
				return fmt.Errorf("merchant %q provider_accounts.%s.%s: %w", slug, key, rail, err)
			}
		}
		if strings.TrimSpace(cfg.AccountID) == "" {
			return fmt.Errorf("merchant %q provider_accounts.%s.%s.account_id is required (auto-discovery removed; declare account_id in the manifest)", slug, key, rail)
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
