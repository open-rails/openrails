package app

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/checkout"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
)

type solanaEligibilityAdapter struct {
	service *checkout.CheckoutService
}

func (a *solanaEligibilityAdapter) CheckPurchaseEligibility(ctx context.Context, userID string, priceID uuid.UUID) (*solanamodule.PurchaseEligibilityResult, error) {
	result, err := a.service.CheckPurchaseEligibility(ctx, userID, priceID)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &solanamodule.PurchaseEligibilityResult{Status: "allowed"}, nil
	}
	return &solanamodule.PurchaseEligibilityResult{Status: string(result.Status), Reason: result.Reason}, nil
}
