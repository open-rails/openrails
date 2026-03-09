package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/query"
)

// -------------------------------- Admin Subscriptions --------------------------------

// AdminGetSubscriptions returns a paginated list of all subscriptions.
func (s *Service) AdminGetSubscriptions(ctx context.Context, opts AdminGetSubscriptionsOptions) (*PaginatedResult[Subscription], error) {
	adminSubscriptions, err := s.requireAdminSubscriptionService()
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	queryOpts := &query.QueryOptions[services.GetSubscriptionsFilters]{
		Limit:  limit,
		Offset: offset,
		Filters: services.GetSubscriptionsFilters{
			UserID:    opts.UserID,
			Status:    opts.Status,
			Processor: opts.Processor,
		},
	}

	subs, total, err := adminSubscriptions.GetAllSubscriptions(ctx, queryOpts)
	if err != nil {
		return nil, fmt.Errorf("get subscriptions: %w", err)
	}

	result := make([]Subscription, 0, len(subs))
	for _, sub := range subs {
		result = append(result, adminSubscriptionFromResponse(sub))
	}

	return &PaginatedResult[Subscription]{
		Data:       result,
		TotalItems: total,
		Limit:      limit,
		Offset:     offset,
	}, nil
}

// AdminGetSubscription returns a single subscription by ID.
func (s *Service) AdminGetSubscription(ctx context.Context, subscriptionID uuid.UUID) (*Subscription, error) {
	adminSubscriptions, err := s.requireAdminSubscriptionService()
	if err != nil {
		return nil, err
	}
	if subscriptionID == uuid.Nil {
		return nil, fmt.Errorf("subscription_id required")
	}

	sub, err := adminSubscriptions.GetSubscriptionByID(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}

	result := adminSubscriptionFromResponse(sub)
	return &result, nil
}

// AdminCancelSubscription cancels a subscription by ID.
func (s *Service) AdminCancelSubscription(ctx context.Context, subscriptionID uuid.UUID, reason string) error {
	adminSubscriptions, err := s.requireAdminSubscriptionService()
	if err != nil {
		return err
	}
	if subscriptionID == uuid.Nil {
		return fmt.Errorf("subscription_id required")
	}

	return adminSubscriptions.CancelSubscription(ctx, subscriptionID, reason)
}

// -------------------------------- Admin Payments --------------------------------

// AdminGetPayments returns a paginated list of all payments.
func (s *Service) AdminGetPayments(ctx context.Context, opts AdminGetPaymentsOptions) (*PaginatedResult[Payment], error) {
	paymentsService, err := s.requirePaymentService()
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	filters := services.GetPaymentsFilters{
		UserID:    opts.UserID,
		Processor: opts.Processor,
	}
	if opts.SubscriptionID != nil {
		filters.SubscriptionID = opts.SubscriptionID.String()
	}
	if opts.StartDate != nil {
		filters.StartDate = opts.StartDate
	}
	if opts.EndDate != nil {
		filters.EndDate = opts.EndDate
	}
	queryOpts := query.QueryOptions[services.GetPaymentsFilters]{
		Limit:   limit,
		Offset:  offset,
		Filters: filters,
	}

	payments, total, err := paymentsService.GetPayments(ctx, queryOpts)
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

// AdminGetPayment returns a single payment by ID with refund details.
func (s *Service) AdminGetPayment(ctx context.Context, paymentID uuid.UUID) (*Payment, error) {
	paymentsService, err := s.requirePaymentService()
	if err != nil {
		return nil, err
	}
	if paymentID == uuid.Nil {
		return nil, fmt.Errorf("payment_id required")
	}

	payment, refunds, err := paymentsService.GetByIDWithDetails(ctx, paymentID)
	if err != nil {
		return nil, err
	}

	result := paymentFromModel(payment)
	if len(refunds) > 0 {
		result.Refunds = make([]Payment, 0, len(refunds))
		for _, r := range refunds {
			result.Refunds = append(result.Refunds, paymentFromModel(r))
		}
	}
	return &result, nil
}

// AdminRefundPayment issues a refund for a payment.
func (s *Service) AdminRefundPayment(ctx context.Context, paymentID uuid.UUID, req RefundPaymentRequest) (*Payment, error) {
	paymentsService, err := s.requirePaymentService()
	if err != nil {
		return nil, err
	}
	if paymentID == uuid.Nil {
		return nil, fmt.Errorf("payment_id required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	if req.RefundTransactionID == "" {
		return nil, fmt.Errorf("refund_transaction_id required")
	}

	refund, err := paymentsService.Refund(ctx, paymentID, req.RefundTransactionID, req.Amount)
	if err != nil {
		return nil, err
	}

	result := paymentFromModel(refund)
	return &result, nil
}

// AdminGetUserPayments returns payments for a specific user.
func (s *Service) AdminGetUserPayments(ctx context.Context, userID string, opts AdminGetPaymentsOptions) (*PaginatedResult[Payment], error) {
	if _, err := s.requirePaymentService(); err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	opts.UserID = userID
	return s.AdminGetPayments(ctx, opts)
}

// AdminCreateOffChannelPayment creates a payment record for an off-channel payment.
func (s *Service) AdminCreateOffChannelPayment(ctx context.Context, req AdminCreateOffChannelPaymentRequest) (*Payment, error) {
	paymentsService, err := s.requirePaymentService()
	if err != nil {
		return nil, err
	}

	req.UserID = strings.TrimSpace(req.UserID)
	if req.UserID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	if req.Currency == "" {
		return nil, fmt.Errorf("currency required")
	}
	if req.TransactionID == "" {
		return nil, fmt.Errorf("transaction_id required")
	}
	if req.Processor == "" {
		return nil, fmt.Errorf("processor required")
	}

	// Parse price ID if provided
	var priceID *uuid.UUID
	if req.PriceID != "" {
		id, err := api.ParsePriceID(req.PriceID)
		if err != nil {
			return nil, fmt.Errorf("invalid price_id")
		}
		priceID = &id
	}

	now := time.Now().UTC()
	payment := &models.Payment{
		ID:            uuid.New(),
		UserID:        req.UserID,
		Amount:        req.Amount,
		ListAmount:    req.Amount,
		Currency:      strings.ToLower(req.Currency),
		TransactionID: req.TransactionID,
		Processor:     models.Processor(req.Processor),
		PurchasedAt:   now,
		CreatedAt:     now,
	}

	if priceID != nil {
		payment.PriceID = *priceID
	}

	if err := paymentsService.Create(ctx, payment); err != nil {
		return nil, fmt.Errorf("create payment: %w", err)
	}

	result := paymentFromModel(payment)
	return &result, nil
}

// -------------------------------- Admin Users --------------------------------

// AdminGetUserBillingProfile returns a user's complete billing profile.
func (s *Service) AdminGetUserBillingProfile(ctx context.Context, userID string) (*AdminUserProfile, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	profile := &AdminUserProfile{
		UserID:        userID,
		Subscriptions: []Subscription{},
		Payments:      []Payment{},
		Entitlements:  []EntitlementRecord{},
	}

	now := time.Now().UTC()

	// Get active subscription
	if s.rt.SubscriptionService != nil {
		sub, err := s.rt.SubscriptionService.GetActiveSubscription(ctx, userID)
		if err == nil && sub != nil {
			profile.Subscriptions = []Subscription{adminSubscriptionFromModel(sub)}
		}
	}

	// Get entitlements
	if s.rt.EntitlementService != nil {
		ents, err := s.rt.EntitlementService.ListActiveRecords(ctx, userID, now)
		if err == nil {
			for _, e := range ents {
				profile.Entitlements = append(profile.Entitlements, entitlementRecordFromModel(&e))
			}
		}
	}

	// Get payments
	if s.rt.PaymentService != nil {
		payments, err := s.rt.PaymentService.GetByUserID(ctx, userID)
		if err == nil {
			for _, p := range payments {
				profile.Payments = append(profile.Payments, paymentFromModel(p))
			}
		}
	}

	return profile, nil
}

// -------------------------------- Admin Entitlements --------------------------------

// AdminGetUserEntitlements returns entitlements for a user.
func (s *Service) AdminGetUserEntitlements(ctx context.Context, userID string, at *time.Time) ([]EntitlementRecord, error) {
	entitlements := s.entitlementService()
	if entitlements == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}

	queryTime := time.Now().UTC()
	if at != nil {
		queryTime = *at
	}

	ents, err := entitlements.ListActiveRecords(ctx, userID, queryTime)
	if err != nil {
		return nil, fmt.Errorf("get entitlements: %w", err)
	}

	result := make([]EntitlementRecord, 0, len(ents))
	for _, e := range ents {
		result = append(result, entitlementRecordFromModel(&e))
	}

	return result, nil
}

// AdminGrantEntitlement grants an entitlement to a user.
func (s *Service) AdminGrantEntitlement(ctx context.Context, adminUserID string, req AdminGrantEntitlementRequest) (*EntitlementRecord, error) {
	database, dbErr := s.requireDB()
	if dbErr != nil {
		return nil, dbErr
	}
	entitlements := s.entitlementService()
	if entitlements == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	req.UserID = strings.TrimSpace(req.UserID)
	req.Entitlement = strings.TrimSpace(req.Entitlement)
	adminUserID = strings.TrimSpace(adminUserID)

	if req.UserID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if req.Entitlement == "" {
		return nil, fmt.Errorf("entitlement required")
	}
	if adminUserID == "" {
		return nil, fmt.Errorf("admin_user_id required")
	}

	now := time.Now().UTC()

	// Create admin grant record for audit
	adminGrant := &models.AdminGrant{
		ID:        uuid.New(),
		UserID:    req.UserID,
		GrantedBy: adminUserID,
		Reason:    req.Reason,
		CreatedAt: now,
	}

	// Calculate duration if end time provided
	if req.EndAt != nil && !req.EndAt.IsZero() {
		days := int(req.EndAt.Sub(now).Hours() / 24)
		if days > 0 {
			adminGrant.DurationDays = &days
		}
	}

	if _, err := database.GetDB().NewInsert().Model(adminGrant).Exec(ctx); err != nil {
		return nil, fmt.Errorf("create admin grant: %w", err)
	}

	var ent *models.Entitlement
	var err error

	if req.EndAt != nil && !req.EndAt.IsZero() {
		endAt := req.EndAt.UTC()
		if endAt.After(now) {
			ent, err = entitlements.PushNewEntitlement(ctx, services.PushNewEntitlementParams{
				UserID:      req.UserID,
				Entitlement: req.Entitlement,
				EndAt:       &endAt,
				SourceType:  models.EntitlementSourceAdmin,
				SourceID:    adminGrant.ID,
			})
		} else {
			return nil, fmt.Errorf("end_at must be in the future")
		}
	} else {
		ent, err = entitlements.PushNewEntitlement(ctx, services.PushNewEntitlementParams{
			UserID:      req.UserID,
			Entitlement: req.Entitlement,
			Indefinite:  true,
			SourceType:  models.EntitlementSourceAdmin,
			SourceID:    adminGrant.ID,
		})
	}

	if err != nil {
		return nil, err
	}

	result := entitlementRecordFromEntitlement(ent)
	return &result, nil
}

// AdminRevokeEntitlement revokes an entitlement.
func (s *Service) AdminRevokeEntitlement(ctx context.Context, userID string, entitlementID uuid.UUID, req AdminRevokeEntitlementRequest) error {
	entitlements := s.entitlementService()
	if entitlements == nil {
		return fmt.Errorf("billing service: not initialized")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("user_id required")
	}
	if entitlementID == uuid.Nil {
		return fmt.Errorf("entitlement_id required")
	}

	// Verify the entitlement belongs to the user
	ent, err := entitlements.GetByID(ctx, entitlementID)
	if err != nil {
		return fmt.Errorf("entitlement not found")
	}
	if ent.UserID != userID {
		return fmt.Errorf("entitlement does not belong to user")
	}

	return entitlements.RevokeExistingEntitlement(ctx, services.RevokeExistingEntitlementParams{
		EntitlementID: &entitlementID,
		Reason:        models.EntitlementRevokeAdmin,
	})
}

// -------------------------------- Admin Metrics --------------------------------

// AdminGetMetricsSummary returns aggregated billing metrics.
// Returns the raw SummaryResponse slice from the internal service.
func (s *Service) AdminGetMetricsSummary(ctx context.Context, opts MetricsOptions) ([]services.SummaryResponse, error) {
	cfg, err := s.requireAdminMetricsConfig()
	if err != nil {
		return nil, err
	}

	svc := services.NewAdminMetricsService(cfg.ClickHouse)
	dateRange := services.MetricsDateRange{
		Start: opts.DateRange.Start,
		End:   opts.DateRange.End,
	}

	return svc.GetSummary(ctx, dateRange, opts.Currency)
}

// AdminGetMetricsRevenue returns revenue time series data.
// Returns the raw RevenueSeriesResponse slice from the internal service.
func (s *Service) AdminGetMetricsRevenue(ctx context.Context, opts MetricsOptions) ([]services.RevenueSeriesResponse, error) {
	cfg, err := s.requireAdminMetricsConfig()
	if err != nil {
		return nil, err
	}

	svc := services.NewAdminMetricsService(cfg.ClickHouse)
	dateRange := services.MetricsDateRange{
		Start: opts.DateRange.Start,
		End:   opts.DateRange.End,
	}

	return svc.GetRevenueSeries(ctx, dateRange, opts.Granularity, opts.Currency)
}

// AdminGetMetricsSubscriptions returns subscription time series data.
// Returns the raw SubscriptionSeriesResponse slice from the internal service.
func (s *Service) AdminGetMetricsSubscriptions(ctx context.Context, opts MetricsOptions) ([]services.SubscriptionSeriesResponse, error) {
	cfg, err := s.requireAdminMetricsConfig()
	if err != nil {
		return nil, err
	}

	svc := services.NewAdminMetricsService(cfg.ClickHouse)
	dateRange := services.MetricsDateRange{
		Start: opts.DateRange.Start,
		End:   opts.DateRange.End,
	}

	return svc.GetSubscriptionSeries(ctx, dateRange, opts.Granularity, opts.Currency)
}

// AdminGetMetricsProcessors returns per-processor metrics.
// Returns the raw ProcessorMetricsResponse slice from the internal service.
func (s *Service) AdminGetMetricsProcessors(ctx context.Context, opts MetricsOptions) ([]services.ProcessorMetricsResponse, error) {
	cfg, err := s.requireAdminMetricsConfig()
	if err != nil {
		return nil, err
	}

	svc := services.NewAdminMetricsService(cfg.ClickHouse)
	dateRange := services.MetricsDateRange{
		Start: opts.DateRange.Start,
		End:   opts.DateRange.End,
	}

	return svc.GetProcessorMetrics(ctx, dateRange, opts.Currency)
}

// AdminGetMetricsChurn returns churn analysis data.
// Returns the raw ChurnResponse slice from the internal service.
func (s *Service) AdminGetMetricsChurn(ctx context.Context, opts MetricsOptions) ([]services.ChurnResponse, error) {
	cfg, err := s.requireAdminMetricsConfig()
	if err != nil {
		return nil, err
	}

	svc := services.NewAdminMetricsService(cfg.ClickHouse)
	dateRange := services.MetricsDateRange{
		Start: opts.DateRange.Start,
		End:   opts.DateRange.End,
	}

	return svc.GetChurn(ctx, dateRange, opts.Currency)
}

// -------------------------------- Conversion Helpers --------------------------------

func adminSubscriptionFromResponse(resp *services.AdminSubscriptionResponse) Subscription {
	if resp == nil || resp.Subscription == nil {
		return Subscription{}
	}
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
	if sub.PaymentMethodID != nil {
		result.PaymentMethod = &PaymentMethodSummary{
			ID: api.FormatPaymentMethodID(*sub.PaymentMethodID),
		}
	}
	if resp.Price != nil {
		p := priceFromModel(resp.Price)
		result.Price = &p
	}
	return result
}

func adminSubscriptionFromModel(sub *models.Subscription) Subscription {
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
	if sub.PaymentMethodID != nil {
		result.PaymentMethod = &PaymentMethodSummary{
			ID: api.FormatPaymentMethodID(*sub.PaymentMethodID),
		}
	}
	return result
}

func entitlementRecordFromModel(e *models.Entitlement) EntitlementRecord {
	rec := EntitlementRecord{
		ID:          e.ID,
		UserID:      e.UserID,
		Entitlement: e.Entitlement,
		StartAt:     e.StartAt,
		EndAt:       e.EndAt,
		SourceType:  string(e.SourceType),
		SourceID:    e.SourceID,
		RevokedAt:   e.RevokedAt,
		CreatedAt:   e.CreatedAt,
		UpdatedAt:   e.UpdatedAt,
	}
	if e.RevokeReason != nil {
		reason := string(*e.RevokeReason)
		rec.RevokeReason = &reason
	}
	return rec
}

func entitlementRecordFromEntitlement(e *models.Entitlement) EntitlementRecord {
	return entitlementRecordFromModel(e)
}
