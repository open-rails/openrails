package checkout

import (
	"github.com/open-rails/openrails/internal/db/models"
)

func terminalCheckoutStatus(status string) bool {
	switch models.CheckoutAttemptStatus(status) {
	case models.CheckoutAttemptStatusSucceeded, models.CheckoutAttemptStatusFailed, models.CheckoutAttemptStatusExpired, models.CheckoutAttemptStatusCanceled:
		return true
	}
	return false
}
