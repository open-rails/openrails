package merchantbootstrap

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
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
		if strings.TrimSpace(t.DisplayName) == "" {
			return fmt.Errorf("merchant %q display_name is required", slug)
		}
		if host := merchants.NormalizeAPIHost(t.APIHost); host != "" {
			if err := merchants.ValidateAPIHost(host); err != nil {
				return fmt.Errorf("merchant %q api_host: %w", slug, err)
			}
		}
		// The configuration API's own validator: a manifest cannot declare
		// settings the API would refuse.
		if err := service.ValidateMerchantSettings(t.Settings); err != nil {
			return fmt.Errorf("merchant %q settings: %w", slug, err)
		}
		for key, account := range t.PSPs {
			if err := ValidateManifestPSP(slug, key, account); err != nil {
				return err
			}
		}
	}
	return nil
}

func ValidateManifestPSP(slug string, key string, account config.PSPConfig) error {
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

func SolanaSignerConfigured(cfg config.ProviderRailAccountConfig) bool {
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
