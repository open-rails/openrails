package merchantbootstrap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/config"
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
		// The configuration API's own validator: a manifest cannot declare
		// settings the API would refuse.
		if _, err := merchantconfig.Normalize(t.DisplayName, t.MerchantSettings); err != nil {
			return fmt.Errorf("merchant %q settings: %w", slug, err)
		}
		for key, account := range t.PSPs {
			normalized, err := NormalizeManifestPSP(slug, key, account)
			if err != nil {
				return err
			}
			t.PSPs[key] = normalized
		}
	}
	return nil
}

// NormalizeManifestPSP checks a manifest PSP against its rail's schema and
// returns it in its declared environment (the rail's real-money one when
// omitted).
func NormalizeManifestPSP(slug string, key string, cfg config.PSPConfig) (config.PSPConfig, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return cfg, fmt.Errorf("merchant %q psps key is required", slug)
	}
	rail := NormalizeManifestRail(string(cfg.Rail))
	if rail == "" {
		return cfg, fmt.Errorf("merchant %q psps.%s.rail is required", slug, key)
	}
	cfg, err := config.NormalizePSPDeclaration(key, cfg)
	if err != nil {
		return cfg, fmt.Errorf("merchant %q %w", slug, err)
	}
	return cfg, nil
}

func sortedMerchantKeys(in map[string]config.MerchantDeclaration) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
