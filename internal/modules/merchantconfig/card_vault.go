package merchantconfig

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// MaxChargePSPs bounds settings.charge_psps.
const MaxChargePSPs = 20

// CardVault is where the merchant's new cards are saved: a card PSP's key,
// billing.CardVaultPlatform, or "" for the PSP that takes each card.
func CardVault(settings billing.MerchantSettings) string {
	return strings.TrimSpace(settings.CardVault)
}

// PlatformCardVault reports new cards saved in the platform vault.
func PlatformCardVault(settings billing.MerchantSettings) bool {
	return CardVault(settings) == billing.CardVaultPlatform
}

// ChargePSPs are the card PSPs new card charges go to, in order: the declared
// list, else the card vault PSP alone, else none (every armed card PSP).
func ChargePSPs(settings billing.MerchantSettings) []string {
	if len(settings.ChargePSPs) > 0 {
		return settings.ChargePSPs
	}
	if vault := CardVault(settings); vault != "" && vault != billing.CardVaultPlatform {
		return []string{vault}
	}
	return nil
}

func validateCardVault(settings billing.MerchantSettings) error {
	if vault := settings.CardVault; vault != "" && strings.TrimSpace(vault) != vault {
		return fmt.Errorf("card_vault must be a PSP key or %q", billing.CardVaultPlatform)
	}
	if len(settings.ChargePSPs) > MaxChargePSPs {
		return fmt.Errorf("charge_psps lists at most %d PSPs", MaxChargePSPs)
	}
	seen := map[string]bool{}
	for _, key := range settings.ChargePSPs {
		if key == "" || strings.TrimSpace(key) != key || key == billing.CardVaultPlatform {
			return fmt.Errorf("charge_psps must list PSP keys")
		}
		if seen[key] {
			return fmt.Errorf("charge_psps lists %q twice", key)
		}
		seen[key] = true
	}
	return nil
}
