package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/services"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/query"
)

// -------------------------------- Products --------------------------------

// GetProducts returns a paginated list of products.
func (s *Service) GetProducts(ctx context.Context, opts GetProductsOptions) (*PaginatedResult[Product], error) {
	publicSubscriptions, err := s.requirePublicSubscriptionService()
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	result, err := publicSubscriptions.GetProductsPaginated(ctx, opts.IncludeInactive, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("get products: %w", err)
	}

	products := make([]Product, 0, len(result.Products))
	for _, p := range result.Products {
		products = append(products, productFromModel(p))
	}

	return &PaginatedResult[Product]{
		Data:       products,
		TotalItems: result.TotalItems,
		Limit:      limit,
		Offset:     offset,
	}, nil
}

// -------------------------------- Prices --------------------------------

// GetPrices returns a paginated list of prices.
func (s *Service) GetPrices(ctx context.Context, opts GetPricesOptions) (*PaginatedResult[Price], error) {
	prices, err := s.requirePriceService()
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	filter := catalog.PriceFilter{
		Currency: strings.ToLower(opts.Currency),
		Type:     opts.Type,
	}
	if opts.ProductID != nil {
		filter.ProductID = opts.ProductID
	}
	if opts.Active != nil {
		filter.Active = opts.Active
	} else if !opts.IncludeInactive {
		active := true
		filter.Active = &active
	}

	modelPrices, totalItems, err := prices.ListPaginated(ctx, filter, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("get prices: %w", err)
	}

	items := make([]Price, 0, len(modelPrices))
	for _, p := range modelPrices {
		items = append(items, priceFromModel(p))
	}

	return &PaginatedResult[Price]{
		Data:       items,
		TotalItems: totalItems,
		Limit:      limit,
		Offset:     offset,
	}, nil
}

// -------------------------------- Checkout Sessions --------------------------------

// CreateCheckoutSession creates a new checkout session.
func (s *Service) CreateCheckoutSession(ctx context.Context, userID string, req CreateCheckoutSessionRequest) (*CheckoutSession, error) {
	checkoutSessions, err := s.requireCheckoutSessionService()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	svcReq := &services.CheckoutSessionCreateRequest{
		PriceID:        req.PriceID,
		Mode:           req.Mode,
		Metadata:       req.Metadata,
		IdempotencyKey: req.IdempotencyKey,
		Payment: services.CheckoutSessionPaymentRequest{
			Processor:       req.Payment.Processor,
			PaymentMethodID: req.Payment.PaymentMethodID,
			PaymentToken:    req.Payment.PaymentToken,
			TokenSymbol:     req.Payment.TokenSymbol,
			Flow:            req.Payment.Flow,
			Wallet:          req.Payment.Wallet,
			Email:           req.Payment.Email,
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

	user := &services.UserIdentity{ID: userID}
	resp, err := checkoutSessions.CreateSession(ctx, svcReq, user)
	if err != nil {
		return nil, err
	}

	return checkoutSessionFromResponse(resp), nil
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

	user := &services.UserIdentity{ID: userID}
	resp, err := checkoutSessions.GetSession(ctx, sessionID, user)
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

	svcReq := &services.CheckoutSessionConfirmRequest{
		Payment: services.CheckoutSessionConfirmPayment{
			Processor: req.Payment.Processor,
			Signature: req.Payment.Signature,
			Wallet:    req.Payment.Wallet,
		},
	}

	user := &services.UserIdentity{ID: userID}
	resp, err := checkoutSessions.ConfirmSession(ctx, sessionID, svcReq, user)
	if err != nil {
		return nil, err
	}

	return checkoutSessionFromResponse(resp), nil
}

// -------------------------------- Billing Status --------------------------------

// GetBillingStatus returns a user's overall billing status.
func (s *Service) GetBillingStatus(ctx context.Context, userID string) (*BillingStatus, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	status := &BillingStatus{}

	// Get subscription
	if s.rt.UserSubscriptionService != nil {
		resp, err := s.rt.UserSubscriptionService.GetUserSubscription(ctx, userID)
		if err == nil && resp != nil && resp.Subscription != nil {
			status.HasActiveSubscription = resp.Subscription.Status == models.StatusActive
			status.Subscription = subscriptionDetailFromModel(resp)
			if resp.Subscription.CurrentPeriodEndsAt != nil {
				status.NextRenewalAt = resp.Subscription.CurrentPeriodEndsAt
			}
		}
	}

	// Get entitlements
	if s.rt.EntitlementService != nil {
		ents, err := s.rt.EntitlementService.ListActiveEntitlements(ctx, userID, time.Now().UTC())
		if err == nil {
			status.Entitlements = ents
		}
	}

	return status, nil
}

// -------------------------------- Subscriptions --------------------------------

// GetSubscriptions returns a user's subscriptions.
func (s *Service) GetSubscriptions(ctx context.Context, userID string, opts GetSubscriptionsOptions) (*PaginatedResult[Subscription], error) {
	userSubscriptions, err := s.requireUserSubscriptionService()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	queryOpts := &query.QueryOptions[subscriptions.GetSubscriptionsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: subscriptions.GetSubscriptionsFilters{UserID: userID},
	}
	if opts.Status != "" && opts.Status != "all" {
		queryOpts.Filters.Status = opts.Status
	}

	subs, total, err := userSubscriptions.GetUserSubscriptionHistory(ctx, userID, queryOpts)
	if err != nil {
		return nil, fmt.Errorf("get subscriptions: %w", err)
	}

	result := make([]Subscription, 0, len(subs))
	for _, sub := range subs {
		result = append(result, subscriptionFromModel(sub))
	}

	return &PaginatedResult[Subscription]{
		Data:       result,
		TotalItems: total,
		Limit:      limit,
		Offset:     offset,
	}, nil
}

// CancelSubscription cancels a user's active subscription.
func (s *Service) CancelSubscription(ctx context.Context, userID string, req CancelSubscriptionRequest) (*CancelSubscriptionResult, error) {
	userSubscriptions, err := s.requireUserSubscriptionService()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	err = userSubscriptions.CancelUserSubscription(ctx, userID, req.Feedback)
	if err != nil {
		var ccbillErr *services.CCBillCancelError
		if errors.As(err, &ccbillErr) {
			return &CancelSubscriptionResult{
				Success: false,
				Message: ccbillErr.Message,
			}, nil
		}
		return nil, err
	}

	return &CancelSubscriptionResult{
		Success: true,
		Message: "Subscription cancelled successfully",
	}, nil
}

// ResumeSubscription resumes a cancelled subscription (if supported).
// Note: Resume functionality is not currently supported by the underlying lifecycle service.
func (s *Service) ResumeSubscription(ctx context.Context, userID string) (*ResumeSubscriptionResult, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	// Resume is not currently supported - subscriptions that are cancelled
	// cannot be resumed. Users need to create a new subscription.
	return nil, fmt.Errorf("resume subscription not supported: please create a new subscription")
}

// UpdateSubscriptionPaymentMethod updates the payment method for a subscription.
func (s *Service) UpdateSubscriptionPaymentMethod(ctx context.Context, userID string, req UpdateSubscriptionPaymentMethodRequest) (*UpdateSubscriptionPaymentMethodResult, error) {
	subscriptions, paymentMethods, err := s.requireSubscriptionAndPaymentMethodServices()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	subID, err := uuid.Parse(req.SubscriptionID)
	if err != nil {
		subID, err = api.ParseSubscriptionID(req.SubscriptionID)
		if err != nil {
			return nil, fmt.Errorf("invalid subscription_id")
		}
	}

	pmID, err := uuid.Parse(req.PaymentMethodID)
	if err != nil {
		pmID, err = api.ParsePaymentMethodID(req.PaymentMethodID)
		if err != nil {
			return nil, fmt.Errorf("invalid payment_method_id")
		}
	}

	// Verify ownership and update
	sub, err := subscriptions.GetByID(ctx, subID)
	if err != nil {
		return nil, fmt.Errorf("subscription not found")
	}
	if sub.UserID != userID {
		return nil, fmt.Errorf("subscription does not belong to user")
	}

	pm, err := paymentMethods.GetByID(ctx, pmID)
	if err != nil {
		return nil, fmt.Errorf("payment method not found")
	}
	if pm.UserID != userID {
		return nil, fmt.Errorf("payment method does not belong to user")
	}

	// Update subscription payment method
	sub.PaymentMethodID = &pmID
	if err := subscriptions.Update(ctx, sub); err != nil {
		return nil, fmt.Errorf("update subscription: %w", err)
	}

	return &UpdateSubscriptionPaymentMethodResult{
		Success:         true,
		Message:         "Payment method updated successfully",
		SubscriptionID:  api.FormatSubscriptionID(subID),
		PaymentMethodID: api.FormatPaymentMethodID(pmID),
	}, nil
}

// -------------------------------- Payments --------------------------------

// GetPayments returns a user's payments.
func (s *Service) GetPayments(ctx context.Context, userID string, opts GetPaymentsOptions) (*PaginatedResult[Payment], error) {
	userSubscriptions, err := s.requireUserSubscriptionService()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	queryOpts := &query.QueryOptions[payments.GetPaymentsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: payments.GetPaymentsFilters{UserID: userID},
	}

	payments, total, err := userSubscriptions.GetUserPayments(ctx, userID, queryOpts)
	if err != nil {
		return nil, fmt.Errorf("get payments: %w", err)
	}

	result := make([]Payment, 0, len(payments))
	for _, p := range payments {
		result = append(result, paymentFromModel(p))
	}

	return &PaginatedResult[Payment]{
		Data:       result,
		TotalItems: total,
		Limit:      limit,
		Offset:     offset,
	}, nil
}

// -------------------------------- Payment Methods --------------------------------

// GetPaymentMethods returns a user's payment methods.
func (s *Service) GetPaymentMethods(ctx context.Context, userID string, opts GetPaymentMethodsOptions) (*PaginatedResult[PaymentMethod], error) {
	paymentMethods, err := s.requirePaymentMethodService()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	methods, total, err := paymentMethods.ListByUserID(ctx, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("get payment methods: %w", err)
	}

	result := make([]PaymentMethod, 0, len(methods))
	for _, pm := range methods {
		result = append(result, paymentMethodFromModel(pm))
	}

	return &PaginatedResult[PaymentMethod]{
		Data:       result,
		TotalItems: total,
		Limit:      limit,
		Offset:     offset,
	}, nil
}

// CreatePaymentMethod creates a new payment method.
func (s *Service) CreatePaymentMethod(ctx context.Context, userID string, req CreatePaymentMethodRequest) (*PaymentMethod, error) {
	vaults, err := s.requireVaultService()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	user := &services.UserIdentity{ID: userID}
	pm, err := vaults.CreateVault(ctx, user.ID, &payments.CreateVaultRequest{
		PaymentToken: req.PaymentToken,
		FirstName:    req.FirstName,
		LastName:     req.LastName,
		Address1:     req.Address1,
		City:         req.City,
		State:        req.State,
		Zip:          req.Zip,
		Country:      req.Country,
		Phone:        req.Phone,
		Email:        req.Email,
		Company:      req.Company,
		Address2:     req.Address2,
		Provider:     req.Provider,
		LastFour:     req.LastFour,
		CardType:     req.CardType,
		ExpiryDate:   req.ExpiryDate,
	})
	if err != nil {
		return nil, err
	}

	result := paymentMethodFromModel(pm)
	return &result, nil
}

// UpdatePaymentMethod updates an existing payment method.
func (s *Service) UpdatePaymentMethod(ctx context.Context, userID string, paymentMethodID uuid.UUID, req UpdatePaymentMethodRequest) (*PaymentMethod, error) {
	vaults, paymentMethods, err := s.requireVaultAndPaymentMethodServices()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if paymentMethodID == uuid.Nil {
		return nil, fmt.Errorf("payment_method_id required")
	}

	// Get the existing payment method and verify ownership
	pm, err := paymentMethods.GetByID(ctx, paymentMethodID)
	if err != nil {
		return nil, fmt.Errorf("payment method not found")
	}
	if pm.UserID != userID {
		return nil, fmt.Errorf("payment method does not belong to user")
	}

	// Build update request
	updateReq := &payments.UpdateVaultRequest{
		PaymentToken: &req.PaymentToken,
		FirstName:    req.FirstName,
		LastName:     req.LastName,
		Address1:     req.Address1,
		City:         req.City,
		State:        req.State,
		Zip:          req.Zip,
		Country:      req.Country,
		Phone:        req.Phone,
		Email:        req.Email,
		Company:      req.Company,
		Address2:     req.Address2,
		Provider:     req.Provider,
	}

	pm, err = vaults.UpdateVault(ctx, pm, updateReq)
	if err != nil {
		return nil, err
	}

	result := paymentMethodFromModel(pm)
	return &result, nil
}

// DeletePaymentMethod deletes (deactivates) a payment method.
func (s *Service) DeletePaymentMethod(ctx context.Context, userID string, paymentMethodID uuid.UUID) error {
	vaults, paymentMethods, err := s.requireVaultAndPaymentMethodServices()
	if err != nil {
		return err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("user_id required")
	}
	if paymentMethodID == uuid.Nil {
		return fmt.Errorf("payment_method_id required")
	}

	pm, err := paymentMethods.GetByID(ctx, paymentMethodID)
	if err != nil {
		return fmt.Errorf("payment method not found")
	}
	if pm.UserID != userID {
		return fmt.Errorf("payment method does not belong to user")
	}

	return vaults.DeleteVault(ctx, pm)
}

// -------------------------------- Notifications --------------------------------

// GetNotifications returns a user's notifications.
func (s *Service) GetNotifications(ctx context.Context, userID string, opts GetNotificationsOptions) (*PaginatedResult[Notification], error) {
	userSubscriptions, err := s.requireUserSubscriptionService()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	queryOpts := &query.QueryOptions[services.GetNotificationsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: services.GetNotificationsFilters{UserID: userID, Seen: opts.Seen},
	}

	notifications, total, err := userSubscriptions.GetUserNotifications(ctx, userID, queryOpts)
	if err != nil {
		return nil, fmt.Errorf("get notifications: %w", err)
	}

	result := make([]Notification, 0, len(notifications))
	for _, n := range notifications {
		result = append(result, notificationFromModel(n))
	}

	return &PaginatedResult[Notification]{
		Data:       result,
		TotalItems: total,
		Limit:      limit,
		Offset:     offset,
	}, nil
}

// GetUnreadNotificationCount returns the count of unread notifications.
func (s *Service) GetUnreadNotificationCount(ctx context.Context, userID string) (*UnreadNotificationCount, error) {
	userSubscriptions, err := s.requireUserSubscriptionService()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	unread := false
	queryOpts := &query.QueryOptions[services.GetNotificationsFilters]{
		Limit:   1,
		Offset:  0,
		Filters: services.GetNotificationsFilters{UserID: userID, Seen: &unread},
	}

	_, total, err := userSubscriptions.GetUserNotifications(ctx, userID, queryOpts)
	if err != nil {
		return nil, fmt.Errorf("get unread count: %w", err)
	}

	return &UnreadNotificationCount{Count: total}, nil
}

// MarkNotificationRead marks a notification as read.
func (s *Service) MarkNotificationRead(ctx context.Context, userID string, notificationID uuid.UUID) error {
	userSubscriptions, err := s.requireUserSubscriptionService()
	if err != nil {
		return err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("user_id required")
	}
	if notificationID == uuid.Nil {
		return fmt.Errorf("notification_id required")
	}

	return userSubscriptions.MarkNotificationRead(ctx, userID, notificationID)
}

// -------------------------------- Credits (User-facing) --------------------------------

// GetCredits returns all credit balances for a user.
// Note: This queries the database directly since there's no CreditsService.GetAllBalances method.
func (s *Service) GetCredits(ctx context.Context, userID string) ([]CreditBalance, error) {
	database, err := s.requireDB()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	// Query all credit types with user balances using a direct query
	// This matches the pattern used in the HTTP handler
	var rows []struct {
		CreditTypeID  uuid.UUID `bun:"credit_type_id"`
		Name          string    `bun:"name"`
		DisplayName   string    `bun:"display_name"`
		Unit          string    `bun:"unit"`
		DecimalPlaces int       `bun:"decimal_places"`
		Balance       *int64    `bun:"balance"`
		HeldBalance   *int64    `bun:"held_balance"`
	}

	err = database.GetDB().NewSelect().
		TableExpr("billing.credit_types ct").
		ColumnExpr("ct.id as credit_type_id").
		ColumnExpr("ct.name").
		ColumnExpr("ct.display_name").
		ColumnExpr("ct.unit").
		ColumnExpr("ct.decimal_places").
		ColumnExpr("ucb.balance").
		ColumnExpr("ucb.held_balance").
		Join("LEFT JOIN billing.user_credit_balances ucb ON ucb.credit_type_id = ct.id AND ucb.user_id = ?", userID).
		Where("ct.is_active = true").
		Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("get credits: %w", err)
	}

	result := make([]CreditBalance, 0, len(rows))
	for _, row := range rows {
		result = append(result, CreditBalance{
			Type:          row.Name,
			DisplayName:   row.DisplayName,
			Unit:          row.Unit,
			DecimalPlaces: row.DecimalPlaces,
			Balance:       derefInt64(row.Balance),
			HeldBalance:   derefInt64(row.HeldBalance),
		})
	}

	return result, nil
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// GetCreditsByType returns a specific credit balance for a user.
func (s *Service) GetCreditsByType(ctx context.Context, userID, creditType string) (*CreditBalance, error) {
	userID = strings.TrimSpace(userID)
	creditType = strings.TrimSpace(creditType)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if creditType == "" {
		return nil, fmt.Errorf("credit_type required")
	}

	bal, err := s.creditsService().GetBalance(ctx, userID, creditType)
	if err != nil {
		return nil, fmt.Errorf("get credit balance: %w", err)
	}

	ct, err := s.creditsService().GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, fmt.Errorf("credit type not found")
	}

	return &CreditBalance{
		Type:          creditType,
		DisplayName:   ct.DisplayName,
		Unit:          ct.Unit,
		DecimalPlaces: ct.DecimalPlaces,
		Balance:       bal.Balance,
		HeldBalance:   bal.HeldBalance,
	}, nil
}

// GetCreditTransactions returns credit transactions for a user.
func (s *Service) GetCreditTransactions(ctx context.Context, userID, creditType string, opts GetCreditTransactionsOptions) (*PaginatedResult[CreditTransaction], error) {
	userID = strings.TrimSpace(userID)
	creditType = strings.TrimSpace(creditType)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if creditType == "" {
		return nil, fmt.Errorf("credit_type required")
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	transactions, total, err := s.creditsService().GetTransactions(ctx, userID, creditType, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("get credit transactions: %w", err)
	}

	result := make([]CreditTransaction, 0, len(transactions))
	for _, t := range transactions {
		result = append(result, CreditTransaction{
			ID:              t.ID,
			UserID:          t.UserID,
			Amount:          t.Amount,
			TransactionType: t.TransactionType,
			Source:          t.Source,
			SourceID:        t.SourceID,
			ExpiresAt:       t.ExpiresAt,
			Description:     t.Description,
			CreatedAt:       t.CreatedAt,
		})
	}

	return &PaginatedResult[CreditTransaction]{
		Data:       result,
		TotalItems: int64(total),
		Limit:      limit,
		Offset:     offset,
	}, nil
}

// -------------------------------- Solana Tokens --------------------------------

// GetSupportedTokens returns the list of supported Solana tokens with prices.
func (s *Service) GetSupportedTokens(ctx context.Context) (*SupportedTokensResult, error) {
	cfg, err := s.requireConfig()
	if err != nil {
		return nil, err
	}
	solanaProc := cfg.GetSolanaProcessor()
	if solanaProc == nil {
		return nil, fmt.Errorf("solana not configured")
	}

	// This would need to call the Jupiter price API like the handler does
	// For now, return configured tokens without live prices
	tokens := make([]SolanaToken, 0)
	for _, t := range solanaProc.SupportedTokens {
		name := t.Name
		if name == "" {
			name = t.Symbol
		}
		tokens = append(tokens, SolanaToken{
			Symbol:   t.Symbol,
			Name:     name,
			Mint:     t.Mint,
			Decimals: t.Decimals,
			Price:    0, // Would need Jupiter API call
		})
	}

	return &SupportedTokensResult{Tokens: tokens}, nil
}

// -------------------------------- Stripe Portal --------------------------------

// CreateStripePortalSession creates a Stripe customer portal session.
func (s *Service) CreateStripePortalSession(ctx context.Context, userID string, req CreateStripePortalSessionRequest) (*StripePortalSession, error) {
	processorCustomers, cfg, err := s.requireProcessorCustomerAndConfig()
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	customerID, err := processorCustomers.GetCustomerID(ctx, userID, "stripe")
	if err != nil || strings.TrimSpace(customerID) == "" {
		return nil, fmt.Errorf("stripe customer not found")
	}

	returnURL := req.ReturnURL
	if returnURL == "" {
		return nil, fmt.Errorf("return_url required")
	}

	service := &subscriptions.StripePortalService{Config: cfg}
	urlStr, err := service.CreatePortalSession(ctx, customerID, returnURL)
	if err != nil {
		return nil, err
	}

	return &StripePortalSession{RedirectURL: urlStr}, nil
}

// -------------------------------- Conversion Helpers --------------------------------

func productFromModel(p *catalog.PublicProductResponse) Product {
	prices := make([]Price, 0, len(p.Prices))
	for _, pr := range p.Prices {
		prices = append(prices, priceFromModel(pr))
	}
	return Product{
		ID:          api.FormatProductID(p.ID),
		Name:        p.DisplayName,
		Description: p.Description,
		Active:      p.IsActive,
		Created:     api.ToUnix(p.CreatedAt),
		Updated:     api.ToUnix(p.UpdatedAt),
		Prices:      prices,
	}
}

func priceFromModel(p *models.Price) Price {
	var recurring *RecurringInfo
	if p.BillingCycleDays != nil && *p.BillingCycleDays > 0 {
		interval, count := sharedformat.BillingCycleDaysToInterval(*p.BillingCycleDays)
		recurring = &RecurringInfo{
			Interval:      interval,
			IntervalCount: count,
		}
	}

	priceType := "one_time"
	if recurring != nil {
		priceType = "recurring"
	}

	return Price{
		ID:        api.FormatPriceID(p.ID),
		Name:      p.DisplayName,
		Amount:    p.Amount,
		Currency:  p.Currency,
		Type:      priceType,
		Recurring: recurring,
		ProductID: api.FormatProductID(p.ProductID),
		Active:    p.IsActive,
		Created:   api.ToUnix(p.CreatedAt),
	}
}

func subscriptionFromModel(resp *services.UserSubscriptionResponse) Subscription {
	sub := resp.Subscription
	result := Subscription{
		ID:                      api.FormatSubscriptionID(sub.ID),
		Status:                  string(sub.Status),
		Processor:               string(sub.Processor),
		ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
		StartedAt:               api.ToUnix(sub.StartedAt),
		Created:                 api.ToUnix(sub.CreatedAt),
		Updated:                 api.ToUnix(sub.UpdatedAt),
	}
	if sub.EndedAt != nil && !sub.EndedAt.IsZero() {
		ts := sub.EndedAt.Unix()
		result.EndedAt = &ts
	}
	if sub.CurrentPeriodStartsAt != nil && !sub.CurrentPeriodStartsAt.IsZero() {
		ts := sub.CurrentPeriodStartsAt.Unix()
		result.CurrentPeriodStartsAt = &ts
	}
	if sub.CurrentPeriodEndsAt != nil && !sub.CurrentPeriodEndsAt.IsZero() {
		ts := sub.CurrentPeriodEndsAt.Unix()
		result.CurrentPeriodEndsAt = &ts
	}
	if sub.CancelledAt != nil && !sub.CancelledAt.IsZero() {
		ts := sub.CancelledAt.Unix()
		result.CancelledAt = &ts
	}
	if sub.CancelType != nil {
		ct := string(*sub.CancelType)
		result.CancelType = &ct
	}
	if sub.CancelFeedback != nil {
		result.CancelFeedback = sub.CancelFeedback
	}
	if resp.Price != nil {
		p := priceFromModel(resp.Price)
		result.Price = &p
	}
	if sub.PaymentMethodID != nil {
		result.PaymentMethod = &PaymentMethodSummary{
			ID: api.FormatPaymentMethodID(*sub.PaymentMethodID),
		}
	}
	return result
}

func subscriptionDetailFromModel(resp *services.UserSubscriptionResponse) *SubscriptionDetail {
	sub := subscriptionFromModel(resp)
	detail := &SubscriptionDetail{Subscription: sub}
	if resp.Product != nil {
		detail.Product = &Product{
			ID:          api.FormatProductID(resp.Product.ID),
			Name:        resp.Product.DisplayName,
			Description: resp.Product.Description,
			Active:      resp.Product.IsActive,
			Created:     api.ToUnix(resp.Product.CreatedAt),
			Updated:     api.ToUnix(resp.Product.UpdatedAt),
		}
	}
	return detail
}

func paymentFromModel(p *models.Payment) Payment {
	result := Payment{
		ID:            api.FormatPaymentID(p.ID),
		Status:        "succeeded",
		Amount:        p.Amount,
		Currency:      p.Currency,
		UserID:        api.FormatUserID(p.UserID),
		Processor:     string(p.Processor),
		TransactionID: p.TransactionID,
		Created:       api.ToUnix(p.CreatedAt),
	}
	if p.SubscriptionID != nil {
		subID := api.FormatSubscriptionID(*p.SubscriptionID)
		result.SubscriptionID = &subID
	}
	if p.Price != nil {
		price := priceFromModel(p.Price)
		result.Price = &price
	}
	return result
}

func paymentMethodFromModel(pm *models.PaymentMethod) PaymentMethod {
	result := PaymentMethod{
		ID:        api.FormatPaymentMethodID(pm.ID),
		Type:      "card",
		Processor: string(pm.Processor),
		Created:   api.ToUnix(pm.CreatedAt),
	}
	if pm.FailureReason != nil {
		result.FailureReason = pm.FailureReason
	}
	if pm.LastFour != nil || pm.CardType != nil {
		result.Card = &CardDetails{
			Brand: pm.CardType,
			Last4: pm.LastFour,
		}
		if pm.ExpiryDate != nil {
			if month, year, ok := sharedformat.ParseExpiry(*pm.ExpiryDate); ok {
				result.Card.ExpMonth = &month
				result.Card.ExpYear = &year
			}
		}
	}
	return result
}

func notificationFromModel(n *models.NotificationQueue) Notification {
	return Notification{
		ID:      n.ID.String(),
		Type:    string(n.EventType),
		Title:   "", // Would need to be derived from event type
		Message: "", // Would need to be derived from event type
		Seen:    n.IsSeen(),
		Data:    n.Data,
		Created: api.ToUnix(n.CreatedAt),
	}
}

func checkoutSessionFromResponse(resp *services.CheckoutSessionResponse) *CheckoutSession {
	result := &CheckoutSession{
		ID:       resp.ID,
		Status:   resp.Status,
		Mode:     resp.Mode,
		PriceID:  resp.PriceID,
		Metadata: resp.Metadata,
	}
	if resp.ExpiresAt != nil {
		result.ExpiresAt = resp.ExpiresAt.Unix()
	}
	if resp.PaymentID != nil {
		result.PaymentID = resp.PaymentID
	}
	if resp.SubscriptionID != nil {
		result.SubscriptionID = resp.SubscriptionID
	}
	if resp.NextAction != nil && resp.NextAction.RedirectToURL != nil {
		result.URL = &resp.NextAction.RedirectToURL.URL
	}
	result.ProcessorData = map[string]any{
		"processor":       resp.Payment.Processor,
		"reference":       resp.Payment.Reference,
		"transaction_url": resp.Payment.TransactionURL,
		"solana_pay_url":  resp.Payment.SolanaPayURL,
		"redirect_url":    resp.Payment.RedirectURL,
		"transaction_id":  resp.Payment.TransactionID,
	}
	return result
}

// Placeholder for UserIdentity to avoid importing internal package directly in method signatures
// The actual UserIdentity is from internal/services, which we can use since this is in the same module
var _ = sql.ErrNoRows // Keep sql import
