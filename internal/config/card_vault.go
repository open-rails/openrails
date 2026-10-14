package config

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
)

// CardVaultConfig is the operator's card vault for this deployment's
// posture: merchants with settings.card_vault platform store new cards in it,
// each in its own partition. Its keys are in Vault, at
// <kv_mount>/<scope_prefix>/platform_vault/<tenant_id>.
type CardVaultConfig struct {
	// Kind is the vault vendor: basis_theory.
	Kind string `yaml:"kind" json:"kind"`
	// TenantID is the vendor's tenant id.
	TenantID string `yaml:"tenant_id" json:"tenant_id"`
	// KVMount holds the platform credential document. Empty uses vault.kv_mount.
	// Setting this does not select Vault as the merchant configuration source.
	KVMount string `yaml:"kv_mount" json:"kv_mount"`
}

// PlatformCustodianKey is the custodian key of a merchant's identity in the
// platform vault; no merchant custodian takes it.
const PlatformCustodianKey = models.CustodianPlatform

// cardVaultKinds are the vendors a platform vault may be.
var cardVaultKinds = []string{models.CustodianBasisTheory}

func validateCardVault(cfg *Config) error {
	if cfg == nil || cfg.CardVault == nil {
		return nil
	}
	v := cfg.CardVault
	if kind := strings.ToLower(strings.TrimSpace(v.Kind)); !slices.Contains(cardVaultKinds, kind) {
		return fmt.Errorf("card_vault.kind %q: must be one of %s", v.Kind, strings.Join(cardVaultKinds, ", "))
	}
	if v.TenantID == "" || strings.ContainsFunc(v.TenantID, func(r rune) bool { return r == '/' || unicode.IsSpace(r) }) {
		return fmt.Errorf("card_vault.tenant_id %q: must be the vendor's tenant id, without spaces or /", v.TenantID)
	}
	mount, err := CardVaultKVMount(cfg)
	if err != nil {
		return err
	}
	if mount == "" {
		return fmt.Errorf("card_vault needs card_vault.kv_mount or vault.kv_mount: its keys are in Vault")
	}
	return nil
}

// CardVaultKVMount selects the platform credential store without changing
// where merchant configuration lives.
func CardVaultKVMount(cfg *Config) (string, error) {
	if cfg == nil || cfg.CardVault == nil {
		return "", nil
	}
	if raw := cfg.CardVault.KVMount; raw != "" {
		mount := strings.Trim(strings.TrimSpace(raw), "/")
		if mount == "" || path.Clean(mount) != mount || strings.Contains(mount, "..") || strings.ContainsFunc(mount, unicode.IsSpace) {
			return "", fmt.Errorf("card_vault.kv_mount %q must be a plain Vault path", raw)
		}
		return mount, nil
	}
	return MerchantConfigKVMount(cfg), nil
}

// PlatformCustodianAccountID is a merchant's identity in the platform vault:
// the tenant and the merchant's partition.
func PlatformCustodianAccountID(v *CardVaultConfig, merchant uuid.UUID) string {
	return v.TenantID + "/" + merchant.String()
}
