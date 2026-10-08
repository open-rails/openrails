package nmi

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// DeclaredAmount is the schedule's verbatim major-unit amount. A subscription
// amount overrides its plan's amount; a malformed explicit override cannot fall
// back to the shared plan and pretend the subscription matched. The read
// carries no currency: callers parse it in the obligation's currency.
func (sub V5Subscription) DeclaredAmount() string {
	amount := strings.TrimSpace(sub.Amount)
	if amount == "" && sub.Plan != nil {
		amount = strings.TrimSpace(sub.Plan.PlanAmount)
	}
	return amount
}

// SubscriptionAmountMinor interprets DeclaredAmount in the established
// obligation currency. It does not establish that currency.
func SubscriptionAmountMinor(sub V5Subscription, currency string) (moneyutil.Cents, error) {
	minor, err := moneyutil.DecimalToRailMinor(currency, sub.DeclaredAmount())
	if err != nil || minor <= 0 {
		return 0, fmt.Errorf("subscription %s has no exact positive amount in %s", sub.ID, currency)
	}
	return minor, nil
}
