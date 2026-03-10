package app

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/payments"
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

type solanaPurchaseRegistrarAdapter struct {
	service *checkout.CheckoutService
}

func (a *solanaPurchaseRegistrarAdapter) RegisterPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*solanamodule.RegisterPurchaseResult, error) {
	result, err := a.service.RegisterPurchase(ctx, req)
	if err != nil {
		return nil, err
	}
	return &solanamodule.RegisterPurchaseResult{
		PaymentID:    result.PaymentID,
		Entitlements: result.Entitlements,
	}, nil
}
