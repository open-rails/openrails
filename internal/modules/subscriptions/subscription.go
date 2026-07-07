package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/query"
	log "github.com/sirupsen/logrus"
)

type GetSubscriptionsFilters struct {
	UserID          string     `form:"user_id"`
	Status          string     `form:"status"`
	PriceID         uuid.UUID  `form:"price_id"`
	Rail            string     `form:"rail"`
	CreatedAfter    *time.Time `form:"created_after" time_format:"2006-01-02"`
	CreatedBefore   *time.Time `form:"created_before" time_format:"2006-01-02"`
	CancelledAfter  *time.Time `form:"cancelled_after" time_format:"2006-01-02"`
	CancelledBefore *time.Time `form:"cancelled_before" time_format:"2006-01-02"`
	ExpiresBefore   *time.Time `form:"expires_before" time_format:"2006-01-02"`
	SortBy          string     `form:"sort_by"`    // created_at (default), expires_at, cancelled_at
	SortOrder       string     `form:"sort_order"` // asc, desc (default)
}

type SubscriptionService struct {
	db                   *db.DB
	subscriptionRepo     *SubscriptionRepo
	notificationRepo     *NotificationQueueRepo
	clock                clockwork.Clock
	PriceService         *catalog.PriceService
	ProductService       *catalog.ProductService
	PaymentMethodService *paymentmethods.PaymentMethodService
	VaultService         *paymentmethods.VaultService
}

var ErrActiveSubscriptionExists = errors.New("active or pending subscription already exists for this product")

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *SubscriptionService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *SubscriptionService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *SubscriptionService) Clock() clockwork.Clock {
	return s.clock
}

const (
	RailCCBill = "ccbill"
	RailStripe = "stripe"

	CurrencyUSD = "usd"
	CurrencyEUR = "eur"

	BillingCycleMonthly = 30

	WebhookSourceCCBill = "ccbill_webhook"
	WebhookSourceNMI    = "nmi_webhook"
	WebhookSourceSystem = "system"

	EventReasonSubscriptionExpired        = "subscription_expired"
	EventReasonSubscriptionDeletedWebhook = "subscription_deleted_via_webhook"
	EventReasonPaymentDeclined            = "payment_declined"

	StatusMessagePending = "pending"
)

func (s *SubscriptionService) GetUserSubscription(ctx context.Context, userID string) (*models.Subscription, error) {
	return s.GetByUserID(ctx, userID)
}

// CancelUserSubscription cancels a user's subscription
func (s *SubscriptionService) CancelUserSubscription(ctx context.Context, userID string, feedback string) error {
	subscription, err := s.GetByUserID(ctx, userID)
	if err != nil {
		return fmt.Errorf("subscription not found: %w", err)
	}

	if subscription.Status != models.StatusActive {
		return errors.New("subscription is not active")
	}

	now := s.now()
	cancelType := models.CancelTypeUser
	subscription.Status = models.StatusCancelled
	subscription.CancelledAt = &now
	subscription.CancelType = &cancelType
	subscription.ClearRetrySchedule()
	if feedback != "" {
		subscription.CancelFeedback = &feedback
	}

	if err := s.Update(ctx, subscription); err != nil {
		return fmt.Errorf("failed to update subscription: %w", err)
	}

	// Entitlements are managed in lifecycle and user flows

	// Add notification
	notification := &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: identity.CustomerIDFromString(userID).UUID(),
		EventType:  models.NotificationPremiumEnded,
		Data: map[string]any{
			"reason": "user_cancel",
		},
	}
	if err := s.notificationRepo.Create(ctx, notification); err != nil {
		log.WithError(err).Error("failed to create cancellation notification")
	}

	return nil
}

// GetAvailableProducts returns all active products with their prices
func (s *SubscriptionService) GetAvailableProducts(ctx context.Context) ([]*models.Product, error) {
	products, err := s.ProductService.GetActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get active products: %w", err)
	}

	// Load prices for each product
	for _, product := range products {
		prices, err := s.PriceService.GetActiveByProductID(ctx, product.ID)
		if err != nil {
			log.WithFields(log.Fields{
				"product_id": product.ID,
				"error":      err.Error(),
			}).Warn("Failed to load prices for product")
			continue
		}
		product.Prices = prices
	}

	return products, nil
}

// Database exposes the service's DB handle for callers that need to compose
// multiple writes into one transaction (MerchantTx + NewWithPgxTx pattern).
func (s *SubscriptionService) Database() *db.DB { return s.db }

func NewSubscriptionService(
	db *db.DB,
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	paymentMethodService *paymentmethods.PaymentMethodService,
	clocks ...clockwork.Clock,
) *SubscriptionService {
	return &SubscriptionService{
		db:                   db,
		subscriptionRepo:     NewSubscriptionRepo(db),
		notificationRepo:     NewNotificationQueueRepo(db),
		clock:                timeutil.FirstClock(clocks...),
		PriceService:         priceService,
		ProductService:       productService,
		PaymentMethodService: paymentMethodService,
	}
}

func (s *SubscriptionService) Create(ctx context.Context, subscription *models.Subscription) error {
	if subscription == nil {
		return errors.New("subscription is nil")
	}

	if subscription.Status == models.StatusActive || subscription.Status == models.StatusPending || subscription.Status == models.StatusPastDue {
		_, err := s.GetActiveOrPendingByUserIDAndProductID(ctx, subscription.CustomerID.String(), subscription.ProductID)
		if err == nil {
			return ErrActiveSubscriptionExists
		}
		if err != nil && !db.IsNotFound(err) {
			return fmt.Errorf("check existing subscription: %w", err)
		}
	}

	if err := s.subscriptionRepo.Create(ctx, subscription); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "idx_subscriptions_user_product_active_pending") {
			return ErrActiveSubscriptionExists
		}
		return err
	}
	return nil
}

func (s *SubscriptionService) GetByID(ctx context.Context, id uuid.UUID) (*models.Subscription, error) {
	return s.subscriptionRepo.GetByID(ctx, id)
}

func (s *SubscriptionService) GetByUserID(ctx context.Context, id string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetLatestByUserID(ctx, id)
}

func (s *SubscriptionService) GetByUserIDAndPriceID(ctx context.Context, id string, priceID uuid.UUID) (*models.Subscription, error) {
	return s.subscriptionRepo.GetByUserIDAndPriceID(ctx, id, priceID)
}

// GetActiveOrPendingByUserIDAndProductID returns an active or pending subscription for a user and product.
// Uses the denormalized ProductID field for efficient lookup.
func (s *SubscriptionService) GetActiveOrPendingByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error) {
	return s.subscriptionRepo.GetActiveOrPendingByUserIDAndProductID(ctx, userID, productID)
}

// GetActiveOrPendingByUserIDAndTierGroup returns an active or pending subscription for a user
// in the specified tier group. Used to detect upgrade/downgrade scenarios.
// Returns the subscription with its Price and Product loaded.
func (s *SubscriptionService) GetActiveOrPendingByUserIDAndTierGroup(ctx context.Context, userID string, tierGroup string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetActiveOrPendingByUserIDAndTierGroup(ctx, userID, tierGroup)
}

// GetUnknownByUserIDAndProductID returns an `unknown`-status subscription for a
// user and product (#691 checkout guard).
func (s *SubscriptionService) GetUnknownByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error) {
	return s.subscriptionRepo.GetUnknownByUserIDAndProductID(ctx, userID, productID)
}

// GetUnknownByUserIDAndTierGroup returns an `unknown`-status subscription for a
// user in the specified tier group (#691 checkout guard).
func (s *SubscriptionService) GetUnknownByUserIDAndTierGroup(ctx context.Context, userID string, tierGroup string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetUnknownByUserIDAndTierGroup(ctx, userID, tierGroup)
}

func (s *SubscriptionService) Update(ctx context.Context, subscription *models.Subscription) error {
	return s.subscriptionRepo.UpdateAt(ctx, subscription, s.now())
}

// ReplaceForTierChange atomically swaps oldSub (pre-mutated by the caller to
// its cancelled state) for newSub in one transaction, so the one-live-
// subscription-per-(subject, tier-group) unique index is never violated and a
// failure leaves the old subscription active (SEC-10 — nothing to reactivate).
func (s *SubscriptionService) ReplaceForTierChange(ctx context.Context, oldSub, newSub *models.Subscription) error {
	return s.subscriptionRepo.ReplaceForTierChange(ctx, oldSub, newSub, s.now())
}

func (s *SubscriptionService) GetSubscribers(ctx context.Context, params query.QueryOptions[GetSubscriptionsFilters]) ([]*models.Subscription, int64, error) {
	repoParams := query.QueryOptions[SubscriptionFilters]{
		Filters: SubscriptionFilters{
			UserID:          params.Filters.UserID,
			Status:          params.Filters.Status,
			PriceID:         params.Filters.PriceID,
			Rail:            params.Filters.Rail,
			CreatedAfter:    params.Filters.CreatedAfter,
			CreatedBefore:   params.Filters.CreatedBefore,
			CancelledAfter:  params.Filters.CancelledAfter,
			CancelledBefore: params.Filters.CancelledBefore,
			ExpiresBefore:   params.Filters.ExpiresBefore,
			SortBy:          params.Filters.SortBy,
			SortOrder:       params.Filters.SortOrder,
		},
		Limit:  params.Limit,
		Offset: params.Offset,
	}

	return s.subscriptionRepo.GetSubscribers(ctx, repoParams)
}

func (s *SubscriptionService) GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	return s.subscriptionRepo.GetPaginatedByUserID(ctx, userID, page, pageSize)
}

// GetSubscriptionsWithDetailsForUser retrieves subscriptions with related price information for billing history
func (s *SubscriptionService) GetSubscriptionsWithDetailsForUser(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	return s.subscriptionRepo.GetSubscriptionsWithDetailsForUser(ctx, userID, page, pageSize)
}

// GetActiveSubscriptionsByUserID retrieves only active subscriptions for a user
func (s *SubscriptionService) GetActiveSubscriptionsByUserID(ctx context.Context, userID string) ([]models.Subscription, error) {
	return s.subscriptionRepo.GetActiveSubscriptionsByUserID(ctx, userID)
}

// GetSubscriptionsByRailAndUserID retrieves subscriptions filtered by rail
func (s *SubscriptionService) GetSubscriptionsByRailAndUserID(ctx context.Context, userID string, rail models.Rail) ([]models.Subscription, error) {
	return s.subscriptionRepo.GetSubscriptionsByRailAndUserID(ctx, userID, rail)
}

// GetActiveSubscription retrieves the active subscription for a user
func (s *SubscriptionService) GetActiveSubscription(ctx context.Context, userID string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetActiveSubscriptionAt(ctx, userID, s.now())
}

// GetByRailSubscriptionID finds a subscription by rail and rail_subscription_id.
func (s *SubscriptionService) GetByRailSubscriptionID(ctx context.Context, rail, railSubscriptionID string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetByRailSubscriptionID(ctx, rail, railSubscriptionID)
}

func (s *SubscriptionService) GetByRailMetadataValue(ctx context.Context, rail, key, value string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetByRailMetadataValue(ctx, rail, key, value)
}

// GetActiveSubscriptionsByRail gets all active subscriptions for a rail
func (s *SubscriptionService) GetActiveSubscriptionsByRail(ctx context.Context, rail string) ([]*models.Subscription, error) {
	return s.subscriptionRepo.GetActiveSubscriptionsByRail(ctx, rail)
}

// Delete removes a subscription from the database permanently
func (s *SubscriptionService) Delete(ctx context.Context, id uuid.UUID) error {
	return s.subscriptionRepo.Delete(ctx, id)
}
