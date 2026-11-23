package catalogrules

import "github.com/open-rails/openrails/catalog"

// OwnershipPrice is what derivation reads of one live price.
type OwnershipPrice struct {
	Recurring      bool
	AccessDuration bool
}

// DeriveOwnership is a product's rule: the declared one, else consumable for a
// one-time credit product, unique for recurring credit, extend when every live price is one-time with an access
// duration, unique otherwise.
func DeriveOwnership(declared catalog.Ownership, creditGrant bool, prices []OwnershipPrice) catalog.Ownership {
	if declared != "" {
		return declared
	}
	if creditGrant {
		for _, price := range prices {
			if price.Recurring {
				return catalog.OwnershipUnique
			}
		}
		return catalog.OwnershipConsumable
	}
	extend := len(prices) > 0
	for _, p := range prices {
		if p.Recurring || !p.AccessDuration {
			extend = false
		}
	}
	if extend {
		return catalog.OwnershipExtend
	}
	return catalog.OwnershipUnique
}
