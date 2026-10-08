package checkout

import (
	"fmt"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

func validateOrderRenewal(price *models.Price, autoRenew *bool, rail models.Rail) error {
	if autoRenew == nil {
		return nil
	}
	if !price.IsRecurring() {
		if *autoRenew {
			return fmt.Errorf("%w: a one-time price cannot renew", ErrCheckoutAttemptValidation)
		}
		return nil
	}
	if !*autoRenew && rails.NewSubscriptionFor(rail) != rails.NewSubscriptionEngine {
		return fmt.Errorf("%w: auto_renew false is not supported for %s; this provider cannot guarantee a single term before payment", ErrCheckoutAttemptValidation, rail)
	}
	return nil
}

func sessionAutoRenew(session *models.CheckoutAttempt) bool {
	renew, present := session.RailState["auto_renew"].(bool)
	return !present || renew
}

func orderAutoRenewInput(session *models.CheckoutAttempt) *bool {
	renew, present := session.RailState["auto_renew"].(bool)
	if !present {
		return nil
	}
	return &renew
}
