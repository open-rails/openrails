package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/money"
)

// -------------------------------- Checkout Sessions --------------------------------

// ListCheckoutRailOptions returns the locally ready payment-provider choices
// for a price. It does not probe remote providers or mutate billing state.
func (s *Service) ListCheckoutRailOptions(ctx context.Context, priceRef string) ([]CheckoutRailOption, error) {
	checkoutSessions, err := s.requireCheckoutSessionService()
	if err != nil {
		return nil, err
	}
	priceRef = strings.TrimSpace(priceRef)
	if priceRef == "" {
		return nil, fmt.Errorf("price reference is required")
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: database unavailable")
	}
	var options []checkout.CheckoutRailOption
	err = rt.DB.RunInMerchantConn(ctx, func(scopedCtx context.Context) error {
		var listErr error
		options, listErr = checkoutSessions.ListCheckoutRailOptions(scopedCtx, priceRef)
		return listErr
	})
	if err != nil {
		return nil, fmt.Errorf("list checkout rail options: %w", err)
	}
	result := make([]CheckoutRailOption, 0, len(options))
	for _, option := range options {
		pspID := ""
		if option.PSPID != uuid.Nil {
			pspID = option.PSPID.String()
		}
		result = append(result, CheckoutRailOption{
			Selector: option.Selector,
			PSPID:    pspID,
			Rail:     option.Rail,
			Mode:     option.Mode,
		})
	}
	return result, nil
}

// CreateCheckoutSessionForCustomer creates a checkout session with host-resolved
// identity attributes for rails that require them.
func (s *Service) CreateCheckoutSessionForCustomer(ctx context.Context, customer CheckoutCustomerIdentity, req CreateCheckoutSessionRequest) (*CheckoutSession, error) {
	checkoutSessions, err := s.requireCheckoutSessionService()
	if err != nil {
		return nil, err
	}
	user, err := checkoutUserIdentity(customer)
	if err != nil {
		return nil, err
	}

	svcReq := &checkout.CheckoutSessionCreateRequest{
		PriceID:        req.PriceID.String(),
		SubscriptionID: req.SubscriptionID.String(),
		NewPriceID:     req.NewPriceID.String(),
		Mode:           req.Mode,
		Metadata:       req.Metadata,
		IdempotencyKey: req.IdempotencyKey,
		SuccessURL:     req.SuccessURL,
		CancelURL:      req.CancelURL,
		Payment: checkout.CheckoutSessionPaymentRequest{
			PSPID:           req.Payment.PSPID,
			Rail:            req.Payment.Rail,
			PaymentMethodID: req.Payment.PaymentMethodID.String(),
			PaymentToken:    req.Payment.PaymentToken,
			TokenSymbol:     req.Payment.TokenSymbol,
			Flow:            req.Payment.Flow,
			Wallet:          req.Payment.Wallet,
			Email:           req.Payment.Email,
			NameOnCard:      req.Payment.NameOnCard,
			FirstName:       req.Payment.FirstName,
			LastName:        req.Payment.LastName,
			Address1:        req.Payment.Address1,
			City:            req.Payment.City,
			State:           req.Payment.State,
			Zip:             req.Payment.Zip,
			Country:         req.Payment.Country,
			LastFour:        req.Payment.LastFour,
			CardType:        req.Payment.CardType,
			ExpiryDate:      req.Payment.ExpiryDate,
		},
	}

	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: database unavailable")
	}
	var resp *checkout.CheckoutSessionResponse
	err = rt.DB.RunInMerchantConn(ctx, func(scopedCtx context.Context) error {
		var createErr error
		resp, createErr = checkoutSessions.CreateSession(scopedCtx, svcReq, user)
		return createErr
	})
	if err != nil {
		return nil, err
	}

	return checkoutSessionFromResponse(resp), nil
}

func checkoutUserIdentity(customer CheckoutCustomerIdentity) (*checkout.UserIdentity, error) {
	if customer.ID.IsZero() {
		return nil, fmt.Errorf("user_id required")
	}

	var email *string
	if verifiedEmail := strings.TrimSpace(customer.VerifiedEmail); verifiedEmail != "" {
		email = &verifiedEmail
	}
	return &checkout.UserIdentity{
		ID:       customer.ID.String(),
		Email:    email,
		Username: strings.TrimSpace(customer.Username),
	}, nil
}

// GetCheckoutSession retrieves a checkout session by ID.
func (s *Service) GetCheckoutSession(ctx context.Context, userID string, sessionID uuid.UUID) (*CheckoutSession, error) {
	checkoutSessions, err := s.requireCheckoutSessionService()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if sessionID == uuid.Nil {
		return nil, fmt.Errorf("session_id required")
	}

	user := &checkout.UserIdentity{ID: userID}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: database unavailable")
	}
	var resp *checkout.CheckoutSessionResponse
	err = rt.DB.RunInMerchantConn(ctx, func(scopedCtx context.Context) error {
		var getErr error
		resp, getErr = checkoutSessions.GetSession(scopedCtx, sessionID, user)
		return getErr
	})
	if err != nil {
		return nil, err
	}

	return checkoutSessionFromResponse(resp), nil
}

// ConfirmCheckoutSession confirms a checkout session (primarily for Solana).
func (s *Service) ConfirmCheckoutSession(ctx context.Context, userID string, sessionID uuid.UUID, req ConfirmCheckoutSessionRequest) (*CheckoutSession, error) {
	checkoutSessions, err := s.requireCheckoutSessionService()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if sessionID == uuid.Nil {
		return nil, fmt.Errorf("session_id required")
	}

	svcReq := &checkout.CheckoutSessionConfirmRequest{
		Payment: checkout.CheckoutSessionConfirmPayment{
			Capture:   req.Payment.Capture,
			Rail:      req.Payment.Rail,
			Signature: req.Payment.Signature,
			Wallet:    req.Payment.Wallet,
		},
	}

	user := &checkout.UserIdentity{ID: userID}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: database unavailable")
	}
	var resp *checkout.CheckoutSessionResponse
	err = rt.DB.RunInMerchantConn(ctx, func(scopedCtx context.Context) error {
		var confirmErr error
		resp, confirmErr = checkoutSessions.ConfirmSession(scopedCtx, sessionID, svcReq, user)
		return confirmErr
	})
	if err != nil {
		return nil, err
	}

	return checkoutSessionFromResponse(resp), nil
}

// -------------------------------- Billing Status --------------------------------

// ResolveEffectiveTier returns THE effective tier for a user within one tier
// group (or#912): the highest-ranked non-archived product whose declared
// entitlements intersect the user's active entitlement windows now.
// Overlapping active entitlements (mid-upgrade) deterministically resolve to
// the highest tier_rank — never an error. Returns (nil, nil) when the user
// holds no active entitlement in the group; the host applies its own default.
func (s *Service) ResolveEffectiveTier(ctx context.Context, userID, group string) (*EffectiveTier, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if s.rt.EntitlementService == nil {
		return nil, fmt.Errorf("entitlement service unavailable")
	}
	tier, err := s.rt.EntitlementService.ResolveEffectiveTier(ctx, userID, group, s.now().UTC())
	if err != nil {
		return nil, err
	}
	if tier == nil {
		return nil, nil
	}
	return &EffectiveTier{
		Group:       tier.TierGroup,
		Entitlement: tier.Entitlement,
		DisplayName: tier.ProductDisplayName,
		TierRank:    tier.TierRank,
		ProductID:   openrails.ProductID(tier.ProductID),
		ProductKey:  tier.ProductKey,
	}, nil
}

// -------------------------------- Credits (User-facing) --------------------------------

// GetCreditsByType returns the user's money balance for the requested currency.
func (s *Service) GetCreditsByType(ctx context.Context, userID, currency string) (*CreditBalance, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	currency, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}

	payer := identity.CustomerIDFromString(userID)
	if payer.IsZero() {
		return nil, fmt.Errorf("payer could not be resolved from subject")
	}
	bal, err := s.moneyService().GetBalanceForCustomer(ctx, payer, currency)
	if err != nil {
		return nil, fmt.Errorf("get credit balance: %w", err)
	}
	decimals, err := money.CurrencyDecimals(bal.Currency)
	if err != nil {
		return nil, err
	}
	return &CreditBalance{
		Currency:      bal.Currency,
		DisplayName:   bal.Currency,
		Unit:          bal.Currency,
		DecimalPlaces: decimals,
		Balance:       bal.Balance,
		HeldBalance:   bal.HeldBalance,
	}, nil
}

// -------------------------------- Conversion Helpers --------------------------------

func checkoutSessionFromResponse(resp *checkout.CheckoutSessionResponse) *CheckoutSession {
	result := &CheckoutSession{
		Capture:         resp.Capture,
		PaymentMethodID: resp.PaymentMethodID,
		ID:              resp.ID,
		Status:          resp.Status,
		Mode:            resp.Mode,
		PriceID:         resp.PriceID,
		Amount:          resp.Amount,
		Currency:        resp.Currency,
		CreatedAt:       resp.CreatedAt,
		ExpiresAt:       resp.ExpiresAt,
		Metadata:        resp.Metadata,
	}
	if resp.PaymentID != nil {
		result.PaymentID = resp.PaymentID
	}
	if resp.SubscriptionID != nil {
		result.SubscriptionID = resp.SubscriptionID
	}
	if resp.URL != "" {
		result.URL = &resp.URL
	} else if resp.NextAction != nil && resp.NextAction.RedirectToURL != nil {
		result.URL = &resp.NextAction.RedirectToURL.URL
	}
	result.RailData = map[string]any{
		"rail":            resp.Payment.Rail,
		"reference":       resp.Payment.Reference,
		"transaction_url": resp.Payment.TransactionURL,
		"solana_pay_url":  resp.Payment.SolanaPayURL,
		"redirect_url":    resp.Payment.RedirectURL,
		"transaction_id":  resp.Payment.TransactionID,
	}
	return result
}

// Placeholder for UserIdentity to avoid importing internal package directly in method signatures.
// The actual UserIdentity lives in internal/modules/checkout.
var _ = sql.ErrNoRows // Keep sql import
