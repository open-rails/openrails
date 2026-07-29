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

type CheckoutPaymentMethodResolver struct {
	PaymentMethodService *paymentmethods.PaymentMethodService
	RailPaymentMethodService         *paymentmethods.RailPaymentMethodService
}

func NewCheckoutPaymentMethodResolver(paymentMethodService *paymentmethods.PaymentMethodService, railPMService *paymentmethods.RailPaymentMethodService) *CheckoutPaymentMethodResolver {
	return &CheckoutPaymentMethodResolver{
		PaymentMethodService: paymentMethodService,
		RailPaymentMethodService:         railPMService,
	}
}

// ResolvePaymentMethod returns the vault handle pair (customer-scope vault id +
// instrument-scope billing id, #682), the resolved payment method row (#297:
// carries the stored-credential replay references), and whether it was freshly
// vaulted from a token in THIS request (created=true — the caller may
// best-effort clean it up on a verified-clean decline). The billing id is ""
// for pre-#682 rows that never recorded one.
func (s *CheckoutPaymentMethodResolver) ResolvePaymentMethod(ctx context.Context, req *CheckoutRequest, user *UserIdentity, target railTarget) (railCustomerRef, billingID string, pm *models.PaymentMethod, created bool, err error) {
	if req.PaymentMethodID != "" {
		pmID, err := api.ParsePaymentMethodID(req.PaymentMethodID)
		if err != nil {
			return "", "", nil, false, fmt.Errorf("invalid payment_method_id: %w", err)
		}

		pm, err := s.PaymentMethodService.ValidatePaymentMethodOperation(ctx, pmID, user.ID)
		if err != nil {
			return "", "", nil, false, fmt.Errorf("invalid payment method: %w", err)
		}

		if !rails.IsNMI(pm.Rail) {
			return "", "", nil, false, errors.New("payment method is not compatible with card payments")
		}
		if !rails.SameRail(pm.Rail, models.Rail(target.Rail)) {
			return "", "", nil, false, errors.New("payment method belongs to a different payment provider")
		}

		// NMI-backed only (checked above): vault id = customer-scope handle,
		// billing id = instrument-scope handle (targets the exact card, #682).
		return pm.RailCustomerRef, pm.RailMethodRef, pm, false, nil
	}

	if req.PaymentToken == "" {
		return "", "", nil, false, errors.New("payment_method_id or payment_token is required")
	}
	if s.RailPaymentMethodService == nil {
		return "", "", nil, false, errors.New("vault service unavailable")
	}

	pmNew, err := s.RailPaymentMethodService.CreatePaymentMethod(ctx, user.ID, &paymentmethods.CreatePaymentMethodRequest{
		PaymentToken: req.PaymentToken,
		Provider:     target.PSP,
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
		return "", "", nil, false, err
	}

	return pmNew.RailCustomerRef, pmNew.RailMethodRef, pmNew, true, nil
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
