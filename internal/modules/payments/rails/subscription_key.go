package rails

import (
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
)

// SubscriptionKey is a provider subscription id's identity on its rail, the
// rule of billing.rail_subscription_key: a CCBill id is a number, so its
// leading zeros do not count; any other rail's id is as spelled.
func SubscriptionKey(rail, ref string) string {
	if rail == string(models.RailCCBill) {
		return strings.TrimLeft(ref, "0")
	}
	return ref
}

// SameSubscription reports whether two provider ids name one subscription on
// the rail.
func SameSubscription(rail, a, b string) bool {
	return a != "" && b != "" && SubscriptionKey(rail, a) == SubscriptionKey(rail, b)
}
