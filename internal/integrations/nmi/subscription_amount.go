package nmi

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// SubscriptionAmountMinor interprets the provider's decimal amount using the
// established obligation currency. It does not establish that currency: the
// subscription read currently exposes no currency field. A subscription amount
// overrides its plan's amount; a malformed explicit override cannot fall back
// to the shared plan and pretend the subscription matched.
func SubscriptionAmountMinor(sub V5Subscription, currency string) (moneyutil.Cents, error) {
	amount := strings.TrimSpace(sub.Amount)
	if amount == "" && sub.Plan != nil {
		amount = strings.TrimSpace(sub.Plan.PlanAmount)
	}
	minor, ok := exactMinorAmount(amount, currency)
	if !ok || minor <= 0 {
		return 0, fmt.Errorf("subscription %s has no exact positive amount in %s", sub.ID, currency)
	}
	return moneyutil.Cents(minor), nil
}
