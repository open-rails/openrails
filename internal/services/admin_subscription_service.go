package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/processors"
	"github.com/open-rails/openrails/pkg/query"
	log "github.com/sirupsen/logrus"
)

// Additional sentinel errors for admin operations
var (
	ErrUserNotFound      = errors.New("user not found")
	ErrRoleNotFound      = errors.New("role not found")
	ErrRoleGrantNotFound = errors.New("role grant not found")
)

// AdminSubscriptionService handles administrative subscription operations
type AdminSubscriptionService struct {
	SubscriptionService *subscriptions.SubscriptionService
	ProductService      *catalog.ProductService
	PriceService        *catalog.PriceService
	EntitlementService  *entitlements.EntitlementService
	NotificationService *NotificationService
	PaymentService      *payments.PaymentService
	NMIClients          map[string]*nmi.NMIClient
	EventLogService     *EventLogService
	Clock               clockwork.Clock
	// No user directory enrichment; IdP subject is stored on subscription
}

// SetClock sets the clock for this service. Used for testing.
func (s *AdminSubscriptionService) SetClock(c clockwork.Clock) {
	s.Clock = c
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *AdminSubscriptionService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// AdminSubscriptionResponse represents a subscription with enriched admin data
type AdminSubscriptionResponse struct {
	*models.Subscription
	//Product  *models.Product   `json:"product,omitempty"`
	Price    *models.Price     `json:"price,omitempty"`
	Payments []*models.Payment `json:"payments,omitempty"`
}

// GetAllSubscriptions retrieves all subscriptions with filtering (admin)
func (s *AdminSubscriptionService) GetAllSubscriptions(ctx context.Context, queryOpts *query.QueryOptions[subscriptions.GetSubscriptionsFilters]) ([]*AdminSubscriptionResponse, int64, error) {
	subscriptions, total, err := s.SubscriptionService.GetSubscribers(ctx, *queryOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get subscriptions: %w", err)
	}

	responses := make([]*AdminSubscriptionResponse, len(subscriptions))
	for i, sub := range subscriptions {
		responses[i] = &AdminSubscriptionResponse{
			Subscription: sub,
		}

		// No user enrichment (IdP-managed)

		// Enrich with price and product data if available
		if price, err := s.PriceService.GetByID(ctx, sub.PriceID); err == nil {
			responses[i].Price = price

			if product, err := s.ProductService.GetByID(ctx, price.ProductID); err == nil {
				responses[i].Product = product
			}
		}
	}

	return responses, total, nil
}

// GetSubscriptionByID retrieves a specific subscription with full details (admin)
func (s *AdminSubscriptionService) GetSubscriptionByID(ctx context.Context, subscriptionID uuid.UUID) (*AdminSubscriptionResponse, error) {
	subscription, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		return nil, fmt.Errorf("subscription not found: %w", err)
	}

	response := &AdminSubscriptionResponse{
		Subscription: subscription,
		Payments:     []*models.Payment{},
	}

	// Enrich with price and product data if available
	if price, err := s.PriceService.GetByID(ctx, subscription.PriceID); err == nil {
		response.Price = price

		if product, err := s.ProductService.GetByID(ctx, price.ProductID); err == nil {
			response.Product = product
		}
	}

	// Include payment history for this subscription
	if s.PaymentService != nil {
		payments, err := s.PaymentService.GetByUserID(ctx, subscription.UserID)
		if err == nil {
			// Filter to only payments for this subscription
			for _, p := range payments {
				if p.SubscriptionID != nil && *p.SubscriptionID == subscriptionID {
					response.Payments = append(response.Payments, p)
				}
			}
		}
	}

	return response, nil
}

// UpdateSubscription updates a subscription (admin)
func (s *AdminSubscriptionService) UpdateSubscription(ctx context.Context, subscriptionID uuid.UUID, updates map[string]any) error {
	subscription, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		return fmt.Errorf("subscription not found: %w", err)
	}

	// Apply allowed updates
	for field, value := range updates {
		switch field {
		case "status":
			if status, ok := value.(models.SubscriptionStatus); ok {
				subscription.Status = status
			}
		case "notes":
			if notes, ok := value.(string); ok {
				// Store notes in Metadata JSONB field which is designed for additional metadata
				var responseData map[string]any
				if subscription.Metadata != nil {
					if err := json.Unmarshal(subscription.Metadata, &responseData); err != nil {
						responseData = make(map[string]any)
					}
				} else {
					responseData = make(map[string]any)
				}
				responseData["admin_notes"] = notes
				if newData, err := json.Marshal(responseData); err == nil {
					subscription.Metadata = newData
				}
			}
		}
	}

	if err := s.SubscriptionService.Update(ctx, subscription); err != nil {
		return fmt.Errorf("failed to update subscription: %w", err)
	}

	return nil
}

// cancelWithNMI cancels a subscription with NMI if applicable
func (s *AdminSubscriptionService) cancelWithNMI(subscription *models.Subscription) error {
	if !processors.IsNMIBackedProcessor(subscription.Processor) {
		return nil // Not an NMI-backed subscription, nothing to do
	}

	if s.NMIClients == nil || subscription.ProcessorSubscriptionID == "" {
		return nil // No NMI clients configured or no subscription ID
	}

	// Use processor name to look up NMI client
	provider := strings.ToLower(string(subscription.Processor))

	client, ok := s.NMIClients[provider]
	if !ok {
		log.WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"processor":       provider,
		}).Warn("NMI provider not configured for admin cancel")
		return nil // Log warning but don't fail the cancellation
	}

	if err := client.DeleteRecurringSubscription(subscription.ProcessorSubscriptionID); err != nil {
		return fmt.Errorf("failed to cancel subscription with NMI provider '%s': %w", provider, err)
	}

	return nil
}

// CancelSubscription cancels a subscription (admin)
func (s *AdminSubscriptionService) CancelSubscription(ctx context.Context, subscriptionID uuid.UUID, reason string) error {
	subscription, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		return fmt.Errorf("subscription not found: %w", err)
	}

	if subscription.Status != models.StatusActive {
		return fmt.Errorf("subscription is not active")
	}

	if !processors.IsNMIBackedProcessor(subscription.Processor) {
		return fmt.Errorf("cancel operation not supported for processor '%s'", subscription.Processor)
	}

	// Cancel with payment processor first (NMI)
	if err := s.cancelWithNMI(subscription); err != nil {
		return err
	}

	now := s.now()
	cancelType := models.CancelTypeMerchant
	subscription.Status = models.StatusCancelled
	subscription.CancelledAt = &now
	subscription.CancelType = &cancelType
	if reason != "" {
		subscription.CancelFeedback = &reason
	}

	if err := s.SubscriptionService.Update(ctx, subscription); err != nil {
		return fmt.Errorf("failed to update subscription: %w", err)
	}

	// End entitlements for this subscription now
	if s.EntitlementService != nil {
		names, err := s.EntitlementService.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceSubscription, subscription.ID)
		if err != nil {
			log.WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.UserID,
				"error":           err.Error(),
			}).Error("Failed to list entitlements for subscription during admin cancellation")
		} else {
			for _, entName := range names {
				st := models.EntitlementSourceSubscription
				sid := subscription.ID
				if err := s.EntitlementService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
					UserID:      subscription.UserID,
					Entitlement: entName,
					SourceType:  &st,
					SourceID:    &sid,
					Reason:      models.EntitlementRevokeAdmin,
				}); err != nil {
					log.WithFields(log.Fields{
						"subscription_id": subscription.ID,
						"user_id":         subscription.UserID,
						"entitlement":     entName,
						"error":           err.Error(),
					}).Error("Failed to revoke entitlement during admin subscription cancellation")
				}
			}
		}
	}

	// Add notification
	notification := &models.NotificationQueue{
		ID:        uuid.New(),
		UserID:    subscription.UserID,
		EventType: models.NotificationPremiumEnded,
		Data:      map[string]any{"reason": string(subscriptions.PremiumEndReasonAdmin)},
	}
	if err := s.NotificationService.Create(ctx, notification); err != nil {
		log.WithFields(log.Fields{
			"subscription_id":   subscription.ID,
			"user_id":           subscription.UserID,
			"notification_type": notification.EventType,
			"error":             err.Error(),
		}).Error("Failed to create notification during admin subscription operation")
	}

	s.logCancellationEvent(ctx, subscription, reason)

	return nil
}

// logCancellationEvent sends a ClickHouse subscription_cancelled event for admin-initiated cancels.
func (s *AdminSubscriptionService) logCancellationEvent(ctx context.Context, subscription *models.Subscription, reason string) {
	if s.EventLogService == nil || subscription == nil {
		return
	}

	var procSubID *string
	if subscription.ProcessorSubscriptionID != "" {
		procSubID = &subscription.ProcessorSubscriptionID
	}

	cancelType := ""
	if subscription.CancelType != nil {
		cancelType = string(*subscription.CancelType)
	}

	metadata := map[string]any{
		"source": "admin",
	}
	if cancelType != "" {
		metadata["cancel_type"] = cancelType
	}
	if reason != "" {
		metadata["reason"] = redactPII(reason)
	}

	var priceAmount float64
	priceCurrency := "usd"
	var billingDays uint32
	var productID *uuid.UUID
	var priceID *uuid.UUID
	if subscription.Price != nil {
		priceAmount = float64(subscription.Price.Amount) / 100.0
		priceCurrency = subscription.Price.Currency
		if subscription.Price.BillingCycleDays != nil {
			billingDays = uint32(*subscription.Price.BillingCycleDays)
		}
		productID = &subscription.Price.ProductID
		priceID = &subscription.Price.ID
	}

	data := SubscriptionEventData{
		SubscriptionID:          subscription.ID,
		UserID:                  subscription.UserID,
		EventType:               PaymentEventSubscriptionCancelled,
		Status:                  string(subscription.Status),
		CancelType:              cancelType,
		PriceAmount:             priceAmount,
		PriceCurrency:           priceCurrency,
		BillingCycleDays:        billingDays,
		ProductID:               productID,
		PriceID:                 priceID,
		Processor:               string(subscription.Processor),
		ProcessorSubscriptionID: procSubID,
		Metadata:                CreateMetadataJSON(metadata),
		Timestamp:               s.now().UTC(),
	}

	if err := s.EventLogService.LogSubscriptionEvent(ctx, data); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"user_id":         subscription.UserID,
		}).Warn("Failed to log admin subscription cancellation to ClickHouse")
	}
}

// ExtendSubscription extends a subscription period by days (admin)
func (s *AdminSubscriptionService) ExtendSubscription(ctx context.Context, subscriptionID uuid.UUID, days int, reason string) error {
	return s.ExtendSubscriptionByDuration(ctx, subscriptionID, time.Duration(days)*24*time.Hour)
}

// ExtendSubscriptionByDuration extends a subscription period by a duration (admin)
func (s *AdminSubscriptionService) ExtendSubscriptionByDuration(ctx context.Context, subscriptionID uuid.UUID, duration time.Duration) error {
	subscription, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		return fmt.Errorf("subscription not found: %w", err)
	}

	if subscription.Status != models.StatusActive {
		return fmt.Errorf("subscription is not active")
	}

	if subscription.CurrentPeriodEndsAt != nil {
		newEndTime := subscription.CurrentPeriodEndsAt.Add(duration)
		subscription.CurrentPeriodEndsAt = &newEndTime
	} else {
		now := s.now()
		newEndTime := now.Add(duration)
		subscription.CurrentPeriodEndsAt = &newEndTime
		subscription.CurrentPeriodStartsAt = &now
	}

	if err := s.SubscriptionService.Update(ctx, subscription); err != nil {
		return fmt.Errorf("failed to update subscription: %w", err)
	}

	// Admin actions don't generate user notifications - this is purely for admin operations

	return nil
}

// CreateProduct creates a new product (admin)
func (s *AdminSubscriptionService) CreateProduct(ctx context.Context, product *models.Product) error {
	product.ID = uuid.New()
	return s.ProductService.Create(ctx, product)
}

// UpdateProduct updates a product (admin)
func (s *AdminSubscriptionService) UpdateProduct(ctx context.Context, productID uuid.UUID, updates map[string]any) error {
	product, err := s.ProductService.GetByID(ctx, productID)
	if err != nil {
		return fmt.Errorf("product not found: %w", err)
	}

	// Apply allowed updates
	for field, value := range updates {
		switch field {
		case "name":
			if name, ok := value.(string); ok {
				product.DisplayName = name
			}
		case "description":
			if desc, ok := value.(string); ok {
				product.Description = desc
			}
		case "is_active":
			if active, ok := value.(bool); ok {
				product.IsActive = active
			}
		}
	}

	return s.ProductService.Update(ctx, product)
}

// CreatePrice creates a new price (admin)
func (s *AdminSubscriptionService) CreatePrice(ctx context.Context, price *models.Price) error {
	price.ID = uuid.New()
	return s.PriceService.Create(ctx, price)
}

// GetAllPurchases retrieves all purchases with filtering (admin)
func (s *AdminSubscriptionService) GetAllPurchases(ctx context.Context, queryOpts *query.QueryOptions[payments.GetPaymentsFilters]) ([]*models.Payment, int64, error) {
	purchases, total, err := s.PaymentService.GetPayments(ctx, *queryOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get purchases: %w", err)
	}

	return purchases, total, nil
}

// GetAllNotifications retrieves all notifications with filtering (admin)
func (s *AdminSubscriptionService) GetAllNotifications(ctx context.Context, queryOpts *query.QueryOptions[GetNotificationsFilters]) ([]*models.NotificationQueue, int64, error) {
	notifications, total, err := s.NotificationService.GetNotifications(ctx, *queryOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get notifications: %w", err)
	}

	return notifications, total, nil
}

// SendManualNotification sends a manual notification (admin)
func (s *AdminSubscriptionService) SendManualNotification(ctx context.Context, userID string, eventType models.NotificationEventType, message string) error {
	notification := &models.NotificationQueue{
		ID:        uuid.New(),
		UserID:    userID,
		EventType: eventType,
		Data: map[string]any{
			"message": message,
			"source":  "admin_manual",
		},
	}

	return s.NotificationService.Create(ctx, notification)
}

// NewAdminSubscriptionService creates a new AdminSubscriptionService
func NewAdminSubscriptionService(
	subscriptionService *subscriptions.SubscriptionService,
	productService *catalog.ProductService,
	priceService *catalog.PriceService,
	entitlementService *entitlements.EntitlementService,
	notificationService *NotificationService,
	paymentService *payments.PaymentService,
	nmiClients map[string]*nmi.NMIClient,
) *AdminSubscriptionService {
	return &AdminSubscriptionService{
		SubscriptionService: subscriptionService,
		ProductService:      productService,
		PriceService:        priceService,
		EntitlementService:  entitlementService,
		NotificationService: notificationService,
		PaymentService:      paymentService,
		NMIClients:          nmiClients,
	}
}
