package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/api"
)

type CheckoutVaultService struct {
	PaymentMethodService *paymentmethods.PaymentMethodService
	VaultService         *paymentmethods.VaultService
}

func NewCheckoutVaultService(paymentMethodService *paymentmethods.PaymentMethodService, vaultService *paymentmethods.VaultService) *CheckoutVaultService {
	return &CheckoutVaultService{
		PaymentMethodService: paymentMethodService,
		VaultService:         vaultService,
	}
}

func (s *CheckoutVaultService) ResolveVault(ctx context.Context, req *CheckoutRequest, user *UserIdentity, provider string) (string, *models.PaymentMethod, error) {
	if req.PaymentMethodID != "" {
		pmID, err := api.ParsePaymentMethodID(req.PaymentMethodID)
		if err != nil {
			return "", nil, fmt.Errorf("invalid payment_method_id: %w", err)
		}

		pm, err := s.PaymentMethodService.ValidatePaymentMethodOperation(ctx, pmID, user.ID)
		if err != nil {
			return "", nil, fmt.Errorf("invalid payment method: %w", err)
		}

		if !rails.IsNMI(pm.Rail) {
			return "", nil, errors.New("payment method is not compatible with card payments")
		}
		if !rails.SameRail(pm.Rail, models.Rail(provider)) {
			return "", nil, errors.New("payment method belongs to a different payment provider")
		}

		// NMI-backed only (checked above): the customer vault id is the customer-scope handle.
		return pm.RailCustomerRef, nil, nil
	}

	if req.PaymentToken == "" {
		return "", nil, errors.New("payment_method_id or payment_token is required")
	}
	if s.VaultService == nil {
		return "", nil, errors.New("vault service unavailable")
	}

	pm, err := s.VaultService.CreateVault(ctx, user.ID, &paymentmethods.CreateVaultRequest{
		PaymentToken: req.PaymentToken,
		Provider:     provider,
		FirstName:    ResolveCheckoutFirstName(req, user),
		LastName:     ResolveCheckoutLastName(req),
		Address1:     req.Address1,
		City:         req.City,
		State:        req.State,
		Zip:          req.Zip,
		Country:      req.Country,
		Email:        req.Email,
		LastFour: func() string {
			if len(req.LastFour) > 4 {
				return req.LastFour[len(req.LastFour)-4:]
			}
			return req.LastFour
		}(),
		CardType:   req.CardType,
		ExpiryDate: req.ExpiryDate,
		Metadata: func() map[string]any {
			if req.Metadata == nil {
				return nil
			}
			if v := strings.TrimSpace(req.Metadata["e2e_run_id"]); v != "" {
				return map[string]any{"e2e_run_id": v}
			}
			return nil
		}(),
	})
	if err != nil {
		return "", nil, err
	}

	return pm.RailCustomerRef, pm, nil
}

func ResolveCheckoutFirstName(req *CheckoutRequest, user *UserIdentity) string {
	if req.FirstName != "" {
		return req.FirstName
	}
	if user.Username != "" {
		return user.Username
	}
	return "Customer"
}

func ResolveCheckoutLastName(req *CheckoutRequest) string {
	if req.LastName != "" {
		return req.LastName
	}
	return "Member"
}

func DefaultIfEmpty(value, defaultValue string) string {
	if strings.TrimSpace(value) == "" {
		return defaultValue
	}
	return value
}
