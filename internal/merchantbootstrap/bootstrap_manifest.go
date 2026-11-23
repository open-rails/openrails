package merchantbootstrap

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// BootstrapManifestVersion is the merchant config manifest's version.
const BootstrapManifestVersion = 1

// CheckManifestDeclaration vets one manifest declaration as the
// configuration API would and normalizes its PSPs in place.
func CheckManifestDeclaration(slug string, t config.MerchantDocument) error {
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
