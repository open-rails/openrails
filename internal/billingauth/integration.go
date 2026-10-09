package billingauth

import "github.com/open-rails/openrails/billing"

// Target is the merchant a request acts on, resolved by OpenRails once. Its
// selectors confer no authority. AuthorityGroupID is an optional opaque
// association owned by the standalone control plane.
type Target struct {
	MerchantID billing.MerchantID
	// MerchantSlug is empty when an ID-selected, group-bound directory has no
	// canonical name authority. Authorize by immutable IDs, never a stale name.
	MerchantSlug     string
	AuthorityGroupID string
}
