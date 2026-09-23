package merchantbootstrap

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
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
		if profileURL := strings.TrimSpace(t.Profile.LogoURL); profileURL != "" && !ValidHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.logo_url must be an http or https URL", slug)
		}
		if profileURL := strings.TrimSpace(t.Profile.SupportURL); profileURL != "" && !ValidHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.support_url must be an http or https URL", slug)
		}
		if profileURL := strings.TrimSpace(t.Profile.SignupURL); profileURL != "" && !ValidHTTPURL(profileURL) {
			return fmt.Errorf("merchant %q profile.signup_url must be an http or https URL", slug)
		}
		if err := ValidateManifestInvoice(slug, t.Invoice); err != nil {
			return err
		}
		if err := ValidateManifestWastedWindows(slug, t.DelegatedInvokerWastedSpendWindows); err != nil {
			return err
		}
		// or#288: the routing policy is validated by the SAME normalizer the
		// mode-2 config API uses, so a manifest cannot declare a policy the API
		// would refuse.
		if _, err := merchantconfig.NormalizeCheckoutRouting(CheckoutRoutingRules(t.CheckoutRouting)); err != nil {
			return fmt.Errorf("merchant %q %w", slug, err)
		}
		for key, account := range t.PSPs {
			if err := ValidateManifestPSP(slug, key, account); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidInvoiceBoundaries mirrors money.NormalizeInvoiceBoundary's accepted set
// (kept inline to avoid importing the money module into bootstrap).
var ValidInvoiceBoundaries = map[string]struct{}{
	"calendar_month": {}, "anniversary": {}, "fixed_interval": {},
}

func ValidateManifestInvoice(slug string, inv *InvoiceConfig) error {
	if inv == nil {
		return nil
	}
	if inv.CollectionThreshold != nil && *inv.CollectionThreshold < 0 {
		return fmt.Errorf("merchant %q invoice.collection_threshold must be >= 0", slug)
	}
	if inv.MonthlyFloor != nil && *inv.MonthlyFloor < 0 {
		return fmt.Errorf("merchant %q invoice.monthly_floor must be >= 0", slug)
	}
	if inv.DelinquencyGraceDays != nil && *inv.DelinquencyGraceDays < 0 {
		return fmt.Errorf("merchant %q invoice.delinquency_grace_days must be >= 0", slug)
	}
	if inv.DelinquencyAmountFloor != nil && *inv.DelinquencyAmountFloor < 0 {
		return fmt.Errorf("merchant %q invoice.delinquency_amount_floor must be >= 0", slug)
	}
	if b := strings.ToLower(strings.TrimSpace(inv.BillingPeriodBoundary)); b != "" {
		if _, ok := ValidInvoiceBoundaries[b]; !ok {
			return fmt.Errorf("merchant %q invoice.billing_period_boundary must be calendar_month, anniversary, or fixed_interval", slug)
		}
	}
	return nil
}

func ValidateManifestWastedWindows(slug string, windows []BudgetWindowConfig) error {
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

func ValidateManifestPSP(slug string, key string, account PSPConfig) error {
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
		// #882: environment is DERIVED from test_mode, never declared. The field
		// could only ever agree (a no-op) or disagree (refuse to boot), so it is
		// retired — a manifest that still carries it fails loudly.
		if strings.TrimSpace(cfg.LegacyEnvironment) != "" {
			return fmt.Errorf("merchant %q psps.%s.%s.environment was removed (#882): the environment is derived from test_mode (sandbox => test, live => live) — delete the key", slug, key, rail)
		}
		for secretKey := range cfg.Secrets {
			if _, err := merchants.NormalizePSPSecretKey(rail, secretKey); err != nil {
				return fmt.Errorf("merchant %q accounts.%s.%s: %w", slug, key, rail, err)
			}
		}
		// #710: the per-merchant CCBill webhook IP allowlist is retired (it was
		// parsed and never enforced); the built-in documented CCBill ranges apply.
		if rail == "ccbill" {
			if _, ok := cfg.Settings["allowed_cidrs"]; ok {
				return fmt.Errorf("merchant %q accounts.%s.ccbill.settings.allowed_cidrs was removed (#710): CCBill webhook source IPs are the built-in documented ranges — delete the key", slug, key)
			}
		}
		if rail == "solana" {
			// Solana never needs account_id — it is always derived from the signer's
			// public key, and a declared value is ignored (warned at apply). A signer
			// is required so there is a key to derive from.
			if !SolanaSignerConfigured(cfg) {
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

func SolanaSignerConfigured(cfg ProviderRailAccountConfig) bool {
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

func ValidHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
