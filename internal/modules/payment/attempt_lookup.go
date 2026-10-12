package payment

import (
	"github.com/open-rails/openrails/internal/db/models"
)

func terminalAttemptStatus(status string) bool {
	switch models.OrderAttemptStatus(status) {
	case models.OrderAttemptStatusSucceeded, models.OrderAttemptStatusFailed, models.OrderAttemptStatusExpired, models.OrderAttemptStatusCanceled:
		return true
	}
	return false
}
