package config

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
)

// PlatformCustodianKey is the custodian key of a merchant's identity in the
// platform vault; no merchant custodian takes it.
const PlatformCustodianKey = models.CustodianPlatform

// cardVaultKinds are the vendors a platform vault may be.
var cardVaultKinds = []string{models.CustodianBasisTheory}

func validateCardVault(cfg *billing.Config) error {
	if cfg == nil || cfg.CardVault == (billing.CardVaultConfig{}) {
		return nil
	}
	v := cfg.CardVault
	if kind := strings.ToLower(strings.TrimSpace(string(v.Kind))); !slices.Contains(cardVaultKinds, kind) {
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
func CardVaultKVMount(cfg *billing.Config) (string, error) {
	if cfg == nil || cfg.CardVault == (billing.CardVaultConfig{}) {
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
func PlatformCustodianAccountID(v *billing.CardVaultConfig, merchant uuid.UUID) string {
	return v.TenantID + "/" + merchant.String()
}
