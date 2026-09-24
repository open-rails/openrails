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
func (s *Service) ListCheckoutRailOptions(ctx context.Context, priceID, priceKey string) ([]CheckoutRailOption, error) {
	checkoutSessions, err := s.requireCheckoutSessionService()
	if err != nil {
		return nil, err
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
		options, listErr = checkoutSessions.ListCheckoutRailOptions(scopedCtx, priceID, priceKey)
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
		out := CheckoutRailOption{
			Selector: option.Selector,
			PSPID:    pspID,
			Rail:     option.Rail,
			Mode:     option.Mode,
		}
		if option.Token != "" {
			out.PublicConfig = map[string]string{"token_symbol": option.Token}
		}
		result = append(result, out)
	}
	return result, nil
}

// CreateCheckoutSessionForCustomer creates a checkout session with host-resolved
// identity attributes for rails that require them.
func (s *Service) CreateCheckoutSessionForCustomer(ctx context.Context, customer CheckoutCustomerIdentity, req CreateCheckoutSessionRequest) (*CheckoutSession, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	return s.createCheckoutSessionForCustomer(ctx, customer, req, "", "", "")
}

func (s *Service) LookupCheckoutSessionForCustomer(ctx context.Context, customer CheckoutCustomerIdentity, req CreateCheckoutSessionRequest) (*CheckoutSession, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	user, err := checkoutUserIdentity(customer)
	if err != nil {
		return nil, err
	}
	return s.lookupCheckoutSession(ctx, user, req)
}

func (s *Service) lookupCheckoutSession(ctx context.Context, user *checkout.UserIdentity, req CreateCheckoutSessionRequest) (*CheckoutSession, error) {
	checkoutSessions, err := s.requireCheckoutSessionService()
	if err != nil {
		return nil, err
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var resp *checkout.CheckoutSessionResponse
	err = rt.DB.RunInMerchantConn(ctx, func(scoped context.Context) error {
		svcReq, err := checkoutCreateRequest(req, "", "", "")
		if err != nil {
			return err
		}
		resp, err = checkoutSessions.LookupSession(scoped, svcReq, user)
		return err
	})
	if err != nil {
		return nil, err
	}
	return checkoutSessionFromResponse(resp), nil
}

func (s *Service) CreatePaymentMethodSessionForCustomer(ctx context.Context, req openrails.CreatePaymentMethodSessionRequest) (*CheckoutSession, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	return s.createCheckoutSessionForCustomer(ctx, req.Customer, CreateCheckoutSessionRequest{PaymentOptions: req.PaymentOptions, Metadata: req.Metadata, IdempotencyKey: req.IdempotencyKey}, "payment_method", "", "")
}

func (s *Service) CreateSolanaCancelSessionForCustomer(ctx context.Context, req openrails.CreateSolanaCancelSessionRequest) (*CheckoutSession, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	return s.createCheckoutSessionForCustomer(ctx, req.Customer, CreateCheckoutSessionRequest{PaymentOptions: req.PaymentOptions, Metadata: req.Metadata, IdempotencyKey: req.IdempotencyKey}, "solana_cancel", req.SubscriptionID, "")
}

func (s *Service) CreateSolanaTierChangeSessionForCustomer(ctx context.Context, req openrails.CreateSolanaTierChangeSessionRequest) (*CheckoutSession, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	return s.createCheckoutSessionForCustomer(ctx, req.Customer, CreateCheckoutSessionRequest{PaymentOptions: req.PaymentOptions, Metadata: req.Metadata, IdempotencyKey: req.IdempotencyKey}, "solana_tier_change", req.SubscriptionID, req.NewPriceID)
}

func (s *Service) createCheckoutSessionForCustomer(ctx context.Context, customer CheckoutCustomerIdentity, req CreateCheckoutSessionRequest, mode string, subscriptionID string, newPriceID string) (*CheckoutSession, error) {
	checkoutSessions, err := s.requireCheckoutSessionService()
	if err != nil {
		return nil, err
	}
	user, err := checkoutUserIdentity(customer)
	if err != nil {
		return nil, err
	}

	svcReq, err := checkoutCreateRequest(req, mode, subscriptionID, newPriceID)
	if err != nil {
		return nil, err
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
	customerID, err := openrails.ParseCustomerID(customer.ID)
	if err != nil || customerID.IsZero() {
		return nil, fmt.Errorf("user_id required")
	}

	var email *string
	if verifiedEmail := strings.TrimSpace(customer.VerifiedEmail); verifiedEmail != "" {
		email = &verifiedEmail
	}
	return &checkout.UserIdentity{
		ID:       customerID.String(),
		Email:    email,
		Username: strings.TrimSpace(customer.Username),
	}, nil
}

func (s *Service) GetCheckoutSessionByKey(ctx context.Context, customerID, key, entitlement string) (*CheckoutSession, error) {
	sessions, err := s.requireCheckoutSessionService()
	if err != nil {
		return nil, err
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var response *checkout.CheckoutSessionResponse
	err = rt.DB.RunInMerchantConn(ctx, func(scoped context.Context) error {
		var readErr error
		response, readErr = sessions.GetSessionByKey(scoped, key, entitlement, &checkout.UserIdentity{ID: customerID})
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return checkoutSessionFromResponse(response), nil
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
		ProductID:   openrails.ProductID(tier.ProductID).String(),
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
		PaymentMethodID: checkoutResponseID(resp.PaymentMethodID),
		ID:              resp.ID.String(),
		Status:          resp.Status,
		Mode:            resp.Mode,
		PriceID:         checkoutResponseID(resp.PriceID),
		Amount:          resp.Amount,
		Currency:        resp.Currency,
		CreatedAt:       resp.CreatedAt,
		ExpiresAt:       resp.ExpiresAt,
		Metadata:        resp.Metadata,
		Operation:       resp.Operation,
		Failure:         resp.Failure,
	}
	if resp.PaymentID != nil {
		result.PaymentID = checkoutResponseID(resp.PaymentID)
	}
	if resp.SubscriptionID != nil {
		result.SubscriptionID = checkoutResponseID(resp.SubscriptionID)
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

func checkoutResponseID[T interface{ String() string }](id *T) *string {
	if id == nil {
		return nil
	}
	value := (*id).String()
	return &value
}

func checkoutCreateRequest(req CreateCheckoutSessionRequest, mode, subscriptionID, newPriceID string) (*checkout.CheckoutSessionCreateRequest, error) {
	if raw := req.PaymentOptions.PaymentMethodID; raw != "" {
		id, err := openrails.ParsePaymentMethodID(raw)
		if err != nil || id.IsZero() {
			return nil, fmt.Errorf("%w: invalid payment_method_id", checkout.ErrCheckoutSessionValidation)
		}
	}
	var pspID uuid.UUID
	var err error
	if req.PaymentOptions.PSPID != "" {
		pspID, err = uuid.Parse(req.PaymentOptions.PSPID)
		if err != nil || pspID == uuid.Nil {
			return nil, fmt.Errorf("%w: invalid psp_id", checkout.ErrCheckoutSessionValidation)
		}
	}
	if mode == "" {
		req.PriceID = strings.TrimSpace(req.PriceID)
		if priceID, err := openrails.ParsePriceID(req.PriceID); err == nil && !priceID.IsZero() {
			req.PriceID = priceID.String()
		}
	}
	return &checkout.CheckoutSessionCreateRequest{
		PriceID:        req.PriceID,
		PriceKey:       req.PriceKey,
		Entitlement:    req.Entitlement,
		OfferKind:      req.OfferKind,
		SubscriptionID: subscriptionID,
		NewPriceID:     newPriceID,
		Mode:           mode,
		Metadata:       req.Metadata,
		IdempotencyKey: req.IdempotencyKey,
		SuccessURL:     req.SuccessURL,
		CancelURL:      req.CancelURL,
		Payment: checkout.CheckoutSessionPaymentRequest{
			PSPID:           pspID,
			Rail:            req.PaymentOptions.Rail,
			PaymentMethodID: req.PaymentOptions.PaymentMethodID,
			PaymentToken:    req.PaymentOptions.PaymentToken,
			TokenSymbol:     req.PaymentOptions.TokenSymbol,
			Flow:            req.PaymentOptions.Flow,
			Wallet:          req.PaymentOptions.Wallet,
			Email:           req.PaymentOptions.Email,
			NameOnCard:      req.PaymentOptions.NameOnCard,
			FirstName:       req.PaymentOptions.FirstName,
			LastName:        req.PaymentOptions.LastName,
			Address1:        req.PaymentOptions.Address1,
			City:            req.PaymentOptions.City,
			State:           req.PaymentOptions.State,
			Zip:             req.PaymentOptions.Zip,
			Country:         req.PaymentOptions.Country,
			LastFour:        req.PaymentOptions.LastFour,
			CardType:        req.PaymentOptions.CardType,
			ExpiryDate:      req.PaymentOptions.ExpiryDate,
		},
	}, nil
}
