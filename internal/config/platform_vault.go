package config

import (
	"fmt"
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
	if MerchantConfigKVMount(cfg) == "" {
		return fmt.Errorf("platform_vault needs vault.kv_mount: its keys are in Vault")
	}
	return nil
}

// PlatformCustodianAccountID is a merchant's identity in the platform vault:
// the tenant and the merchant's partition.
func PlatformCustodianAccountID(v *PlatformVaultConfig, merchant uuid.UUID) string {
	return v.TenantID + "/" + merchant.String()
}
