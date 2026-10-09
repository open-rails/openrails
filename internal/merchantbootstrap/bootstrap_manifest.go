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

func ValidateManifestPSP(slug string, key string, cfg config.PSPConfig) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("merchant %q psps key is required", slug)
	}
	rail := NormalizeManifestRail(string(cfg.Rail))
	if rail == "" {
		return fmt.Errorf("merchant %q psps.%s.rail is required", slug, key)
	}
	// #710: the per-merchant CCBill webhook IP allowlist is retired (it was
	// parsed and never enforced); the built-in documented CCBill ranges apply.
	if rail == "ccbill" {
		if _, ok := cfg.Settings["allowed_cidrs"]; ok {
			return fmt.Errorf("merchant %q psps.%s.settings.allowed_cidrs was removed (#710): CCBill webhook source IPs are the built-in documented ranges — delete the key", slug, key)
		}
	}
	if err := config.ValidatePSPKeys(key, cfg); err != nil {
		return fmt.Errorf("merchant %q %w", slug, err)
	}
	if rail == "solana" {
		// Solana never needs account_id — it is always derived from the signer's
		// public key, and a declared value is ignored (warned at apply). A signer
		// is required so there is a key to derive from.
		if !SolanaSignerConfigured(cfg) {
			return fmt.Errorf("merchant %q psps.%s requires a signer (local_keypair private_key or vault_transit)", slug, key)
		}
		return nil
	}
	if strings.TrimSpace(cfg.AccountID) == "" {
		return fmt.Errorf("merchant %q psps.%s.account_id is required (auto-discovery removed; declare account_id in the manifest)", slug, key)
	}
	// #697: rail-specific format doctrine (CCBill ids are dash-joined).
	if err := config.ValidateRailAccountID(models.Rail(rail), strings.TrimSpace(cfg.AccountID)); err != nil {
		return fmt.Errorf("merchant %q psps.%s: %w", slug, key, err)
	}
	return nil
}

func SolanaSignerConfigured(cfg config.PSPConfig) bool {
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
