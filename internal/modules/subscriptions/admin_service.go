package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
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
	SubscriptionService *SubscriptionService
	ProductService      *catalog.ProductService
	PriceService        *catalog.PriceService
	EntitlementService  *entitlements.EntitlementService
	NotificationService *NotificationService
	PaymentService      *payments.PaymentService
	StripeService *StripeService
	clock         clockwork.Clock

	// deferDelete / ccbillCancel enqueue the durable remote-cancel intents for
	// a merchant-initiated cancel (or#896), admin-origin. Injected
	// post-construction in build_runtime, exactly like the user service's.
	deferDelete  DeferredDeleteScheduler
	ccbillCancel CCBillRemoteCancelScheduler
	// No user directory enrichment; IdP subject is stored on subscription
}

// SetDeferredDeleteScheduler injects the admin-origin NMI delete scheduler.
func (s *AdminSubscriptionService) SetDeferredDeleteScheduler(d DeferredDeleteScheduler) {
	s.deferDelete = d
}

// SetCCBillCancelScheduler injects the admin-origin CCBill cancel scheduler.
func (s *AdminSubscriptionService) SetCCBillCancelScheduler(c CCBillRemoteCancelScheduler) {
	s.ccbillCancel = c
}

// SetClock sets the clock for this service. Used for testing.
func (s *AdminSubscriptionService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *AdminSubscriptionService) Clock() clockwork.Clock {
	return s.clock
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *AdminSubscriptionService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
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
func (s *AdminSubscriptionService) GetAllSubscriptions(ctx context.Context, queryOpts *query.QueryOptions[GetSubscriptionsFilters]) ([]*AdminSubscriptionResponse, int64, error) {
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
		payments, err := s.PaymentService.GetByUserID(ctx, subscription.CustomerID.String())
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

// CancelSubscription cancels a subscription (admin/merchant-initiated).
//
// or#896: the rail side rides the SAME durable intent pipeline the
// user-initiated cancel uses (#674 write-through provider intents). It used to
// call the gateway synchronously with no intent and no verify leg, and an
// unresolvable PSP only logged a warning — so the local row flipped to
// cancelled while NMI happily kept rebilling. Now the local cancellation and
// the remote-cancel intent commit in ONE transaction: the row is never
// terminal while the rail-side schedule survives unconfirmed (the
// DeletionScheduledAt marker stays set until the intent's own verify-then-
// execute leg confirms the NMI subscription is gone), and an ambiguous
// provider outcome parks for verification instead of lying.
func (s *AdminSubscriptionService) CancelSubscription(ctx context.Context, subscriptionID uuid.UUID, reason string, revokeAccess bool) error {
	subscription, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		return fmt.Errorf("subscription not found: %w", err)
	}

	if subscription.Status != models.StatusActive {
		return fmt.Errorf("subscription is not active")
	}

	now := s.now()

	// enqueueRemoteIntent commits the rail's durable remote-mutation intent in
	// the SAME transaction as the local cancellation (nil = no remote intent:
	// the rail either has nothing to stop or was cancelled inline above).
	var enqueueRemoteIntent func(ctx context.Context, tx pgx.Tx) error

	switch {
	case rails.IsNMI(subscription.Rail):
		if subscription.RailSubscriptionID != "" {
			if s.deferDelete == nil {
				return fmt.Errorf("nmi remote-delete scheduler unavailable")
			}
			// Marker + intent commit together: the cancellation is not
			// destructive (and not terminal rail-side) until the intent
			// confirms NMI dropped the schedule.
			subscription.DeletionScheduledAt = &now
			enqueueRemoteIntent = func(ctx context.Context, tx pgx.Tx) error {
				return s.deferDelete.WithTx(tx).ScheduleNMIDelete(ctx, subscription.CustomerID.String(), subscription.ID, now)
			}
		}
	case subscription.Rail == models.RailCCBill:
		// or#896: the admin path used to REFUSE CCBill while the findings
		// queue supported it — same operation, two answers. The DataLink
		// cancelSubscription wire is live-verified (#696 Phase 0), so the
		// refusal was stale: drive the same durable intent here.
		if subscription.RailSubscriptionID != "" {
			if s.ccbillCancel == nil {
				return fmt.Errorf("ccbill remote-cancel scheduler unavailable")
			}
			enqueueRemoteIntent = func(ctx context.Context, tx pgx.Tx) error {
				return s.ccbillCancel.WithTx(tx).ScheduleCCBillCancel(ctx, subscription.CustomerID.String(), subscription.ID)
			}
		}
	case subscription.Rail == models.RailStripe:
		if s.StripeService == nil {
			return fmt.Errorf("stripe cancellation service unavailable")
		}
		if err := s.StripeService.CancelSubscription(ctx, subscription.RailSubscriptionID); err != nil {
			return fmt.Errorf("failed to cancel subscription with Stripe: %w", err)
		}
	case subscription.Rail == models.RailSolana:
		return ErrSolanaCancelNeedsWalletSignature
	default:
		return fmt.Errorf("cancel operation not supported for rail '%s'", subscription.Rail)
	}

	cancelType := models.CancelTypeMerchant
	subscription.Status = models.StatusCancelled
	subscription.CancelledAt = &now
	subscription.CancelType = &cancelType
	subscription.ClearRetrySchedule()
	if reason != "" {
		subscription.CancelFeedback = &reason
	}

	if err := s.SubscriptionService.Database().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := db.NewWithPgxTx(tx)
		txSubSvc := NewSubscriptionService(txdb, catalog.NewPriceService(txdb), catalog.NewProductService(txdb), nil, s.clock)
		if err := txSubSvc.Update(ctx, subscription); err != nil {
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		if enqueueRemoteIntent != nil {
			return enqueueRemoteIntent(ctx, tx)
		}
		return nil
	}); err != nil {
		if enqueueRemoteIntent != nil {
			return fmt.Errorf("failed to persist cancellation with remote intent: %w", err)
		}
		return err
	}

	if revokeAccess && s.EntitlementService != nil {
		if err := s.EntitlementService.RevokeSourcesForSubscription(ctx, subscription.CustomerID.String(), subscription.ID, models.EntitlementRevokeAdmin, models.EntitlementSourceSubscription, models.EntitlementSourceGrace); err != nil {
			log.WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.CustomerID.String(),
				"error":           err.Error(),
			}).Error("Failed to revoke entitlements during admin subscription cancellation")
		}
	}

	// Add notification
	notification := &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: subscription.CustomerID,
		EventType:  models.NotificationPremiumEnded,
		Data:       map[string]any{"reason": string(PremiumEndReasonAdmin)},
	}
	if err := s.NotificationService.Create(ctx, notification); err != nil {
		log.WithFields(log.Fields{
			"subscription_id":   subscription.ID,
			"user_id":           subscription.CustomerID.String(),
			"notification_type": notification.EventType,
			"error":             err.Error(),
		}).Error("Failed to create notification during admin subscription operation")
	}

	return nil
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
	if s.EntitlementService != nil && subscription.CurrentPeriodEndsAt != nil {
		if err := s.EntitlementService.ExtendActiveBySubscription(ctx, subscription.ID, *subscription.CurrentPeriodEndsAt); err != nil {
			return fmt.Errorf("failed to extend subscription entitlements: %w", err)
		}
	}

	// Admin actions don't generate user notifications - this is purely for admin operations

	return nil
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
		ID:         uuidutil.NewV7(),
		CustomerID: identity.CustomerIDFromString(userID).UUID(),
		EventType:  eventType,
		Data: map[string]any{
			"message": message,
			"source":  "admin_manual",
		},
	}

	return s.NotificationService.Create(ctx, notification)
}

// NewAdminSubscriptionService creates a new AdminSubscriptionService
func NewAdminSubscriptionService(
	subscriptionService *SubscriptionService,
	productService *catalog.ProductService,
	priceService *catalog.PriceService,
	entitlementService *entitlements.EntitlementService,
	notificationService *NotificationService,
	paymentService *payments.PaymentService,
	clocks ...clockwork.Clock,
) *AdminSubscriptionService {
	return &AdminSubscriptionService{
		SubscriptionService: subscriptionService,
		ProductService:      productService,
		PriceService:        priceService,
		EntitlementService:  entitlementService,
		NotificationService: notificationService,
		PaymentService:      paymentService,
		clock:               timeutil.FirstClock(clocks...),
	}
}
