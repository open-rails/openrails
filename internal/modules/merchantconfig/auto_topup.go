package merchantconfig

import (
	"fmt"
	"github.com/open-rails/openrails/internal/db/models"
)

// AutoTopupSafety returns the effective merchant safety ceiling.
func AutoTopupSafety(policy *models.AutoTopupSafetyPolicy) (models.AutoTopupSafetyPolicy, error) {
	if policy == nil {
		return models.AutoTopupSafetyPolicy{MaxDaily: 3, MaxWeekly: 10, MaxMonthly: 30, DeclinesBeforeDisable: 3}, nil
	}
	if policy.MaxDaily <= 0 || policy.MaxWeekly <= 0 || policy.MaxMonthly <= 0 || policy.DeclinesBeforeDisable <= 0 {
		return models.AutoTopupSafetyPolicy{}, fmt.Errorf("auto_topup_safety limits must be positive")
	}
	return *policy, nil
}
