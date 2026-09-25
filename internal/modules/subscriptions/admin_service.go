package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/billing/lifecycle"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
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
	StripeService       *StripeService
	clock               clockwork.Clock

	// providerCancel queues the durable provider cancel of a
	// merchant-initiated cancel (or#896, #1102), admin-origin.
	providerCancel ProviderCancelScheduler
	// No user directory enrichment; IdP subject is stored on subscription
}

// SetProviderCancelScheduler injects the admin-origin provider-cancel scheduler.
func (s *AdminSubscriptionService) SetProviderCancelScheduler(c ProviderCancelScheduler) {
	s.providerCancel = c
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

	ids := make([]uuid.UUID, 0, len(subscriptions))
	for _, sub := range subscriptions {
		ids = append(ids, sub.PriceID)
	}
	prices, err := s.PriceService.GetWithProductByIDs(ctx, ids)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to load subscription prices: %w", err)
	}
	responses := make([]*AdminSubscriptionResponse, len(subscriptions))
	for i, sub := range subscriptions {
		responses[i] = &AdminSubscriptionResponse{Subscription: sub}
		if price := prices[sub.PriceID]; price != nil {
			responses[i].Price = price
			responses[i].Product = price.Product
		}
	}

	return responses, total, nil
}

// requireSubscription loads a subscription or returns the typed not-found
// refusal; raw driver errors never travel beneath it.
func (s *AdminSubscriptionService) requireSubscription(ctx context.Context, subscriptionID uuid.UUID) (*models.Subscription, error) {
	subscription, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, fmt.Errorf("load subscription %s: %w", subscriptionID, err)
	}
	return subscription, nil
}

// GetSubscriptionByID retrieves a specific subscription with full details (admin)
func (s *AdminSubscriptionService) GetSubscriptionByID(ctx context.Context, subscriptionID uuid.UUID) (*AdminSubscriptionResponse, error) {
	subscription, err := s.requireSubscription(ctx, subscriptionID)
	if err != nil {
		return nil, err
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
	database := s.SubscriptionService.Database()
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := database.NewWithPgxTx(tx)
		subscription, err := s.requireLockedSubscription(ctx, d, subscriptionID)
		if err != nil {
			return err
		}
		// Apply allowed updates
		for field, value := range updates {
			switch field {
			case "status":
				if status, ok := value.(models.SubscriptionStatus); ok {
					subscription.Status = status
					subscription.MarkLifecycleDecision("admin_status_override")
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

		if err := NewSubscriptionRepo(d).UpdateAt(ctx, subscription, s.now()); err != nil {
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		return nil
	})
}

// Partial admin commands must read their input image after taking the same
// subscription lock as payment admission/completion. A metadata/status change
// cannot replay an older price, card, period or pending quote from preflight.
func (s *AdminSubscriptionService) requireLockedSubscription(ctx context.Context, d *db.DB, id uuid.UUID) (*models.Subscription, error) {
	sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, id)
	if db.IsNotFound(err) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load subscription %s: %w", id, err)
	}
	return sub, nil
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
func rebillInFlight(ctx context.Context, d *db.DB, sub *models.Subscription) (bool, error) {
	rows, err := d.Gen(ctx).ListRebillTermOwners(ctx, gen.ListRebillTermOwnersParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		switch row.Status {
		case "pending", "in_flight", "unknown_needs_verify", "failed_retryable":
			return true, nil
		}
	}
	return false, nil
}

// providerCancellable is every state in which a provider schedule may still
// bill: the merchant (or a host's account-deletion callback) can always stop it.
func providerCancellable(status models.SubscriptionStatus) bool {
	return status.Live()
}

func (s *AdminSubscriptionService) CancelSubscription(ctx context.Context, subscriptionID uuid.UUID, reason string, revokeAccess, accountDeletion bool) error {
	subscription, err := s.requireSubscription(ctx, subscriptionID)
	if err != nil {
		return err
	}

	if subscription.CollectionPolicy == models.CollectionPolicyEngine {
		lifecycle := NewSubscriptionLifecycleService(s.SubscriptionService.Database(), nil, nil, s.EntitlementService, s.NotificationService, nil, s.Clock())
		return lifecycle.CancelMembership(ctx, &CancelMembershipParams{SubscriptionID: &subscription.ID, CancelType: models.CancelTypeMerchant, CancelFeedback: &reason, RevokeAccess: revokeAccess})
	}

	if !providerCancellable(subscription.Status) {
		return ErrSubscriptionNotActive
	}
	deleteHeld, err := RequireProviderCancelArmed(ctx, s.SubscriptionService.Database(), subscription, accountDeletion)
	if err != nil {
		return err
	}

	now := s.now()

	// The provider cancel commits in the SAME transaction as the local
	// cancellation: the row is never terminal while the provider schedule
	// survives unqueued, and an unavailable provider only delays the intent.
	switch {
	case rails.IsNMI(subscription.Rail), subscription.Rail == models.RailCCBill, subscription.Rail == models.RailStripe:
		if subscription.RailSubscriptionID != "" && s.providerCancel == nil {
			return fmt.Errorf("provider cancel scheduler unavailable")
		}
	case subscription.Rail == models.RailSolana:
		return ErrSolanaCancelNeedsWalletSignature
	default:
		return fmt.Errorf("%w: %s", ErrCancelUnsupportedOnRail, subscription.Rail)
	}
	remote := subscription.RailSubscriptionID != ""

	observedPSP, observedRail, observedReference := subscription.PspID, subscription.Rail, subscription.RailSubscriptionID

	if err := s.SubscriptionService.Database().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := s.SubscriptionService.Database().NewWithPgxTx(tx)
		var err error
		subscription, err = s.requireLockedSubscription(ctx, txdb, subscriptionID)
		if err != nil {
			return err
		}
		if !providerCancellable(subscription.Status) {
			return ErrSubscriptionNotActive
		}
		// A delinquent member whose OpenRails rebill is in flight is settled
		// first: the cancel waits for the money to resolve, never races it.
		if subscription.Status != models.StatusActive {
			if moving, err := rebillInFlight(ctx, txdb, subscription); err != nil {
				return err
			} else if moving {
				return ErrSubscriptionNotActive
			}
		}
		if subscription.PspID != observedPSP || subscription.Rail != observedRail || subscription.RailSubscriptionID != observedReference {
			return apperr.Conflictf("subscription provider binding changed before cancellation")
		}
		if _, err := Transition(subscription, lifecycle.Cancel{Kind: lifecycle.CancelMerchant, Immediate: revokeAccess, At: now}, now); err != nil {
			return fmt.Errorf("cancel subscription %s: %w", subscription.ID, err)
		}
		if reason != "" {
			subscription.CancelFeedback = &reason
		}

		if remote {
			subscription.DeletionScheduledAt = nil // a merchant's cancel is due now
			if err := s.providerCancel.WithTx(tx).ScheduleProviderCancel(ctx, subscription, now); err != nil {
				return fmt.Errorf("queue provider cancel: %w", err)
			}
		}
		if err := NewSubscriptionRepo(txdb).UpdateAt(ctx, subscription, now); err != nil {
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		// Immediate cancel: access ends in the same transaction as the cancel;
		// otherwise standing access is bounded to the paid period.
		entSvc := entitlements.NewEntitlementService(txdb, s.Clock())
		if revokeAccess {
			if err := entSvc.RevokeSourcesForSubscription(ctx, subscription.CustomerID.String(), subscription.ID, models.EntitlementRevokeAdmin, models.EntitlementSourceSubscription, models.EntitlementSourceGrace); err != nil {
				return fmt.Errorf("revoke access: %w", err)
			}
		} else {
			accessEnd := now
			if subscription.CurrentPeriodEndsAt != nil && subscription.CurrentPeriodEndsAt.After(now) {
				accessEnd = *subscription.CurrentPeriodEndsAt
			}
			if err := entSvc.BoundSubscriptionAccess(ctx, subscription.ID, accessEnd); err != nil {
				return fmt.Errorf("bound subscription access: %w", err)
			}
		}
		if remote && !deleteHeld {
			return resolveProviderCancelHeld(ctx, txdb, subscription)
		}
		return nil
	}); err != nil {
		return err
	}

	// Add notification
	notification := &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: subscription.CustomerID,
		EventType:  models.NotificationPremiumEnded,
		Data:       openrails.NotificationData{Reason: string(PremiumEndReasonAdmin)},
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
	database := s.SubscriptionService.Database()
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := database.NewWithPgxTx(tx)
		subscription, err := s.requireLockedSubscription(ctx, d, subscriptionID)
		if err != nil {
			return err
		}
		if subscription.Status != models.StatusActive {
			return ErrSubscriptionNotActive
		}
		now := s.now()
		end := now.Add(duration)
		if subscription.CurrentPeriodEndsAt != nil {
			end = subscription.CurrentPeriodEndsAt.Add(duration)
		} else {
			subscription.CurrentPeriodStartsAt = &now
		}
		subscription.CurrentPeriodEndsAt = &end
		subscription.MarkLifecycleDecision("admin_extend")
		if err := NewSubscriptionRepo(d).UpdateAt(ctx, subscription, now); err != nil {
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		if s.EntitlementService != nil {
			if err := entitlements.NewEntitlementService(d, s.clock).ExtendActiveBySubscription(ctx, subscription.ID, end); err != nil {
				return fmt.Errorf("failed to extend subscription entitlements: %w", err)
			}
		}
		return nil
	})
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
		Data:       openrails.NotificationData{Message: message, Source: "admin_manual"},
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
