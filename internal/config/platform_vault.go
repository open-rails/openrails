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

// PlatformVaultConfig is the operator's card vault for this deployment's
// posture: merchants with settings.card_vault platform store new cards in it,
// each in its own partition. Its keys are in Vault, at
// <kv_mount>/<scope_prefix>/platform_vault/<tenant_id>.
type PlatformVaultConfig struct {
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

// platformVaultKinds are the vendors a platform vault may be.
var platformVaultKinds = []string{models.CustodianBasisTheory}

func validatePlatformVault(cfg *Config) error {
	if cfg == nil || cfg.PlatformVault == nil {
		return nil
	}
	v := cfg.PlatformVault
	if kind := strings.ToLower(strings.TrimSpace(v.Kind)); !slices.Contains(platformVaultKinds, kind) {
		return fmt.Errorf("platform_vault.kind %q: must be one of %s", v.Kind, strings.Join(platformVaultKinds, ", "))
	}
	if v.TenantID == "" || strings.ContainsFunc(v.TenantID, func(r rune) bool { return r == '/' || unicode.IsSpace(r) }) {
		return fmt.Errorf("platform_vault.tenant_id %q: must be the vendor's tenant id, without spaces or /", v.TenantID)
	}
	if v.KVMount != "" {
		mount := strings.Trim(strings.TrimSpace(v.KVMount), "/")
		if mount == "" || path.Clean(mount) != mount || strings.Contains(mount, "..") || strings.ContainsFunc(mount, unicode.IsSpace) {
			return fmt.Errorf("platform_vault.kv_mount %q must be a plain Vault path", v.KVMount)
		}
	}
	if PlatformVaultKVMount(cfg) == "" {
		return fmt.Errorf("platform_vault needs platform_vault.kv_mount or vault.kv_mount: its keys are in Vault")
	}
	return nil
}

// PlatformVaultKVMount selects the platform credential store without changing
// where merchant configuration lives.
func PlatformVaultKVMount(cfg *Config) string {
	if cfg == nil || cfg.PlatformVault == nil {
		return ""
	}
	if cfg.PlatformVault.KVMount != "" {
		return strings.Trim(strings.TrimSpace(cfg.PlatformVault.KVMount), "/")
	}
	return MerchantConfigKVMount(cfg)
}

// PlatformCustodianAccountID is a merchant's identity in the platform vault:
// the tenant and the merchant's partition.
func PlatformCustodianAccountID(v *PlatformVaultConfig, merchant uuid.UUID) string {
	return v.TenantID + "/" + merchant.String()
}
