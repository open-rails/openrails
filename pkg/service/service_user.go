package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/vault"
	riverjobs "github.com/open-rails/openrails/internal/river"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/query"
	"github.com/riverqueue/river"
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

	svcReq := &checkout.CheckoutSessionCreateRequest{
		PriceID:        req.PriceID,
		Mode:           req.Mode,
		Metadata:       req.Metadata,
		IdempotencyKey: req.IdempotencyKey,
		Payment: checkout.CheckoutSessionPaymentRequest{
			Rail:            req.Payment.Rail,
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

	user := &checkout.UserIdentity{ID: userID}
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

	user := &checkout.UserIdentity{ID: userID}
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

	svcReq := &checkout.CheckoutSessionConfirmRequest{
		Payment: checkout.CheckoutSessionConfirmPayment{
			Rail:      req.Payment.Rail,
			Signature: req.Payment.Signature,
			Wallet:    req.Payment.Wallet,
		},
	}

	user := &checkout.UserIdentity{ID: userID}
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
		ents, err := s.rt.EntitlementService.ListActiveEntitlements(ctx, userID, s.now().UTC())
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
		var ccbillErr *subscriptions.CCBillCancelError
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

// ResumeSubscription resumes a cancelled-but-still-resumable subscription.
//
// It runs the same path as the HTTP resume route: it locates the user's
// resumable subscription, gates on the single shared Resumable predicate, then
// enqueues the same ResumeSubscriptionArgs River job the HTTP handler enqueues —
// so the library and HTTP entrypoints execute identically.
func (s *Service) ResumeSubscription(ctx context.Context, userID string) (*ResumeSubscriptionResult, error) {
	userSubscriptions, err := s.requireUserSubscriptionService()
	if err != nil {
		return nil, err
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.RiverProducer == nil {
		return nil, fmt.Errorf("job queue unavailable")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	now := s.now().UTC()

	// Find the user's most recent resumable subscription. We look at the current
	// subscription first (active/cancelled-in-window), then fall back to recent
	// cancelled history.
	resp, err := userSubscriptions.GetUserSubscription(ctx, userID)
	if err != nil && !repo.IsNotFound(err) {
		return nil, fmt.Errorf("resume subscription: %w", err)
	}

	var target *models.Subscription
	if resp != nil && resp.Subscription != nil && subscriptions.Resumable(resp.Subscription, now) {
		target = resp.Subscription
	} else {
		// Fall back to scanning the user's subscription history for a resumable one.
		queryOpts := &query.QueryOptions[subscriptions.GetSubscriptionsFilters]{
			Limit:   25,
			Offset:  0,
			Filters: subscriptions.GetSubscriptionsFilters{UserID: userID, Status: string(models.StatusCancelled)},
		}
		history, _, herr := userSubscriptions.GetUserSubscriptionHistory(ctx, userID, queryOpts)
		if herr != nil {
			return nil, fmt.Errorf("resume subscription: %w", herr)
		}
		for _, h := range history {
			if h.Subscription != nil && subscriptions.Resumable(h.Subscription, now) {
				target = h.Subscription
				break
			}
		}
	}

	if target == nil {
		return &ResumeSubscriptionResult{
			Success: false,
			Message: "no resumable subscription found",
		}, nil
	}

	if _, err := rt.RiverProducer.Insert(ctx, riverjobs.ResumeSubscriptionArgs{
		UserID:         userID,
		SubscriptionID: target.ID,
	}, &river.InsertOpts{
		Queue: riverjobs.QueueBilling,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: true,
		},
	}); err != nil {
		return nil, fmt.Errorf("resume subscription: enqueue: %w", err)
	}

	return &ResumeSubscriptionResult{
		Success: true,
		Message: "Subscription resume queued",
	}, nil
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
	if sub.CustomerID.String() != userID {
		return nil, fmt.Errorf("subscription does not belong to user")
	}
	if !rails.IsNMIBackedRail(sub.Rail) {
		return nil, fmt.Errorf("only NMI-backed subscriptions can have their payment method updated")
	}
	if sub.Status != models.StatusActive && sub.Status != models.StatusPastDue {
		return nil, fmt.Errorf("cannot update payment method for subscription status %s", sub.Status)
	}

	pm, err := paymentMethods.GetByID(ctx, pmID)
	if err != nil {
		return nil, fmt.Errorf("payment method not found")
	}
	if pm.CustomerID.String() != userID {
		return nil, fmt.Errorf("payment method does not belong to user")
	}
	if !rails.IsNMIBackedRail(pm.Rail) {
		return nil, fmt.Errorf("only NMI-backed payment methods can be used")
	}
	if !rails.SameRail(pm.Rail, sub.Rail) {
		return nil, fmt.Errorf("payment method belongs to a different payment provider")
	}
	railName := strings.ToLower(string(sub.Rail))
	nmiClient, ok := s.rt.NMIClients[railName]
	if !ok {
		return nil, fmt.Errorf("payment rail not available")
	}
	if err := nmiClient.UpdateSubscriptionPaymentSource(sub.RailSubscriptionID, pm.RailCustomerRef); err != nil {
		return nil, fmt.Errorf("update payment method with rail: %w", err)
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

	user := &checkout.UserIdentity{ID: userID}
	pm, err := vaults.CreateVault(ctx, user.ID, &vault.CreateVaultRequest{
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
	if pm.CustomerID.String() != userID {
		return nil, fmt.Errorf("payment method does not belong to user")
	}

	// Build update request
	updateReq := &vault.UpdateVaultRequest{
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
		LastFour:     req.LastFour,
		CardType:     req.CardType,
		ExpiryDate:   req.ExpiryDate,
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
	if pm.CustomerID.String() != userID {
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

	queryOpts := &query.QueryOptions[subscriptions.GetNotificationsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: subscriptions.GetNotificationsFilters{UserID: userID, Seen: opts.Seen},
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
	queryOpts := &query.QueryOptions[subscriptions.GetNotificationsFilters]{
		Limit:   1,
		Offset:  0,
		Filters: subscriptions.GetNotificationsFilters{UserID: userID, Seen: &unread},
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

func (s *Service) GetCredits(ctx context.Context, userID string) ([]CreditBalance, error) {
	return nil, fmt.Errorf("currency required; use GetCreditsByType")
}

// GetCreditsByType returns the user's money balance for the requested currency.
func (s *Service) GetCreditsByType(ctx context.Context, userID, currency string) (*CreditBalance, error) {
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
	decimals, _ := money.CurrencyScale(bal.Currency)

	return &CreditBalance{
		Currency:      bal.Currency,
		DisplayName:   bal.Currency,
		Unit:          bal.Currency,
		DecimalPlaces: decimals,
		Balance:       bal.Balance,
		HeldBalance:   bal.HeldBalance,
	}, nil
}

// GetCreditTransactions returns money transactions for a user in the requested currency.
func (s *Service) GetCreditTransactions(ctx context.Context, userID, currency string, opts GetCreditTransactionsOptions) (*PaginatedResult[CreditTransaction], error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	payer := identity.CustomerIDFromString(userID)
	if payer.IsZero() {
		return nil, fmt.Errorf("payer could not be resolved from subject")
	}
	transactions, total, err := s.moneyService().GetTransactionsByCustomer(ctx, payer, currency, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("get credit transactions: %w", err)
	}

	result := make([]CreditTransaction, 0, len(transactions))
	for _, t := range transactions {
		result = append(result, CreditTransaction{
			ID:              t.ID,
			CustomerID:      t.CustomerID,
			Invoker:         t.Invoker,
			Currency:        t.Currency,
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
	if _, err := s.requireConfig(); err != nil {
		return nil, err
	}
	var solanaProc *config.RailConfig
	if s.rt != nil {
		solanaProc = s.rt.Rails.GetSolanaRail()
	}
	if solanaProc == nil {
		return nil, fmt.Errorf("solana not configured")
	}

	tokens := make([]SolanaToken, 0)
	for symbol, t := range solanaProc.Tokens {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if symbol == "" {
			continue
		}
		name := t.Name
		if name == "" {
			name = symbol
		}
		tokens = append(tokens, SolanaToken{
			Symbol:   symbol,
			Name:     name,
			Mint:     t.Mint,
			Decimals: t.Decimals,
			Price:    0,
		})
	}

	return &SupportedTokensResult{Tokens: tokens}, nil
}

// -------------------------------- Stripe Portal --------------------------------

// CreateStripePortalSession creates a Stripe customer portal session.
func (s *Service) CreateStripePortalSession(ctx context.Context, userID string, req CreateStripePortalSessionRequest) (*StripePortalSession, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.RailCustomerService == nil || rt.Config == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	customerID, err := rt.RailCustomerService.GetCustomerID(ctx, userID, "stripe")
	if err != nil || strings.TrimSpace(customerID) == "" {
		return nil, fmt.Errorf("stripe customer not found")
	}

	returnURL := req.ReturnURL
	if returnURL == "" {
		return nil, fmt.Errorf("return_url required")
	}

	service := &subscriptions.StripePortalService{Config: rt.Config, Rails: rt.Rails}
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
		ID:               api.FormatProductID(p.ID),
		Slug:             p.Slug,
		Name:             p.DisplayName,
		Description:      p.Description,
		EntitlementsSpec: p.EntitlementsSpec,
		CreditsSpec:      creditsSpecFromModel(p.CreditsSpec),
		TierGroup:        p.TierGroup,
		TierRank:         p.TierRank,
		Active:           p.IsPurchasable(),
		Created:          api.ToUnix(p.CreatedAt),
		Updated:          api.ToUnix(p.UpdatedAt),
		Prices:           prices,
	}
}

func creditsSpecFromModel(in models.CreditsSpec) CreditsSpec {
	if in == nil {
		return nil
	}
	out := make(CreditsSpec, len(in))
	for k, v := range in {
		out[k] = CreditGrantSpec{
			Unit:        v.Unit,
			Amount:      v.Amount,
			ExpiresDays: v.ExpiresDays,
			Cadence:     CreditGrantCadence(v.Cadence),
		}
	}
	return out
}

func priceFromModel(p *models.Price) Price {
	var recurring *RecurringInfo
	if p.BillingCycleDays != nil && *p.BillingCycleDays > 0 {
		recurring = &RecurringInfo{
			Interval: sharedformat.BillingCycleDaysToInterval(*p.BillingCycleDays),
		}
	}

	priceType := "one_time"
	if recurring != nil {
		priceType = "recurring"
	}

	return Price{
		ID:         api.FormatPriceID(p.ID),
		UnitAmount: p.Amount,
		Currency:   p.Currency,
		Type:       priceType,
		Recurring:  recurring,
		ProductID:  api.FormatProductID(p.ProductID),
		Active:     p.IsPurchasable(),
		Created:    api.ToUnix(p.CreatedAt),
	}
}

func subscriptionFromModel(resp *subscriptions.UserSubscriptionResponse) Subscription {
	sub := resp.Subscription
	result := Subscription{
		ID:                 api.FormatSubscriptionID(sub.ID),
		Status:             string(sub.Status),
		Rail:               string(sub.Rail),
		RailSubscriptionID: sub.RailSubscriptionID,
		StartedAt:          api.ToUnix(sub.StartedAt),
		Created:            api.ToUnix(sub.CreatedAt),
		Updated:            api.ToUnix(sub.UpdatedAt),
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

	// Resumability surface — derived from the single shared predicate so the
	// library DTO, the HTTP serialization, the resume handler, and the worker all
	// agree (they call the same subscriptions.* helpers).
	now := time.Now().UTC()
	result.Resumable = subscriptions.Resumable(sub, now)
	result.CancelScheduled = subscriptions.CancelScheduled(sub, now)
	result.CancelMode = string(subscriptions.CancelModeFor(sub, now))
	result.CancelPortalURL = subscriptions.CancelPortalURL(sub, now)

	return result
}

func subscriptionDetailFromModel(resp *subscriptions.UserSubscriptionResponse) *SubscriptionDetail {
	sub := subscriptionFromModel(resp)
	detail := &SubscriptionDetail{Subscription: sub}
	if resp.Product != nil {
		detail.Product = &Product{
			ID:          api.FormatProductID(resp.Product.ID),
			Name:        resp.Product.DisplayName,
			Description: resp.Product.Description,
			Active:      resp.Product.IsPurchasable(),
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
		UserID:        api.FormatUserID(p.CustomerID.String()),
		Rail:          string(p.Rail),
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
		ID:      api.FormatPaymentMethodID(pm.ID),
		Type:    "card",
		Rail:    string(pm.Rail),
		Created: api.ToUnix(pm.CreatedAt),
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

func checkoutSessionFromResponse(resp *checkout.CheckoutSessionResponse) *CheckoutSession {
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
