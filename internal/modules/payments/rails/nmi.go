// Package rails provides helpers for identifying rail types and their underlying gateways.
package rails

import (
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// NMIBackedRails is the set of rails that use NMI as their underlying gateway.
// This is derived from config at startup. The key is the lowercase rail name.
var NMIBackedRails = map[string]bool{
	string(models.RailMobius): true, // Default NMI-backed rail
}

// InitNMIBackedRails initializes the NMI-backed rails set from the
// runtime provider credential set.
func InitNMIBackedRails(rails config.RailSet) {
	if rails == nil {
		return
	}

	// Clear and rebuild from config
	NMIBackedRails = make(map[string]bool)

	nmiRails := rails.GetNMIRails()
	for name := range nmiRails {
		key := normalize.Lower(name)
		if key != "" {
			NMIBackedRails[key] = true
		}
	}

	// Ensure mobius is always included as a default if nothing configured
	if len(NMIBackedRails) == 0 {
		NMIBackedRails[string(models.RailMobius)] = true
	}
}

// IsNMIBacked returns true if the given rail uses NMI as its underlying gateway.
// This is the ONLY place in the codebase that should know which rails use NMI.
func IsNMIBacked(rail string) bool {
	key := normalize.Lower(rail)
	return NMIBackedRails[key]
}

func IsConfigured(rails config.RailSet, rail string) bool {
	return rails.GetRailType(normalize.Lower(rail)) != ""
}

// IsNMIBackedRail returns true if the given models.Rail uses NMI as its gateway.
func IsNMIBackedRail(rail models.Rail) bool {
	return IsNMIBacked(string(rail))
}

// OpenRailsDrivenDunning reports whether OpenRails owns the retry timing for a
// rail (so it models grace access as explicit entitlement windows during
// dunning). True for NMI-backed gateways AND Solana recurring (#256/#257) — both
// are charged by an OpenRails worker. Stripe is excluded: it drives its own
// dunning and emits webhooks.
func OpenRailsDrivenDunning(rail models.Rail) bool {
	return IsNMIBackedRail(rail) || rail == models.RailSolana
}

// SameRail reports whether two rail identifiers name the same configured provider.
func SameRail(a, b models.Rail) bool {
	left := normalize.Lower(string(a))
	right := normalize.Lower(string(b))
	return left != "" && left == right
}

// GetNMIBackedRailsList returns a slice of all NMI-backed rail names.
// Useful for database queries that need to filter by NMI-backed rails.
func GetNMIBackedRailsList() []string {
	result := make([]string, 0, len(NMIBackedRails))
	for name := range NMIBackedRails {
		result = append(result, name)
	}
	return result
}
