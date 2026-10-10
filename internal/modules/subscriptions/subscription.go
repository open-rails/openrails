package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	log "github.com/sirupsen/logrus"
)

type GetSubscriptionsFilters struct {
	CustomerID     billing.CustomerID `form:"customer_id"`
	Status         string             `form:"status"`
	PriceID        billing.PriceID    `form:"price_id"`
	Rail           string             `form:"rail"`
	CreatedAfter   *time.Time         `form:"created_after" time_format:"2006-01-02"`
	CreatedBefore  *time.Time         `form:"created_before" time_format:"2006-01-02"`
	CanceledAfter  *time.Time         `form:"canceled_after" time_format:"2006-01-02"`
	CanceledBefore *time.Time         `form:"canceled_before" time_format:"2006-01-02"`
	ExpiresBefore  *time.Time         `form:"expires_before" time_format:"2006-01-02"`
	// Dunning keeps the subscriptions past_due or awaiting_method.
	Dunning bool `form:"dunning"`
	// IDs, when not nil, reads those subscriptions instead, in one page.
	IDs []uuid.UUID `form:"-"`
}

type SubscriptionService struct {
	db                       *db.DB
	subscriptionRepo         *SubscriptionRepo
	notificationRepo         *NotificationQueueRepo
	clock                    clockwork.Clock
	PriceService             *catalog.PriceService
	ProductService           *catalog.ProductService
	PaymentMethodService     *paymentmethods.PaymentMethodService
	RailPaymentMethodService *paymentmethods.RailPaymentMethodService
}

var ErrActiveSubscriptionExists = errors.New("active or pending subscription already exists for this product")

// now returns the service clock's time, or time.Now() without one.
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

	// Uppercase is the canonical internal form and what the DB CHECK accepts.
	// Lowercase belongs only on a rail wire.
	CurrencyUSD = "USD"
	CurrencyEUR = "EUR"

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

// GetAvailableProducts returns all active products with their prices
func (s *SubscriptionService) GetAvailableProducts(ctx context.Context) ([]*models.Product, error) {
	products, err := s.ProductService.GetActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get active products: %w", err)
	}

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
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "subscriptions_customer_id_product_id_key" {
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
// user and product (checkout guard).
func (s *SubscriptionService) GetUnknownByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error) {
	return s.subscriptionRepo.GetUnknownByUserIDAndProductID(ctx, userID, productID)
}

// GetUnknownByUserIDAndTierGroup returns an `unknown`-status subscription for a
// user in the specified tier group (checkout guard).
func (s *SubscriptionService) GetUnknownByUserIDAndTierGroup(ctx context.Context, userID string, tierGroup string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetUnknownByUserIDAndTierGroup(ctx, userID, tierGroup)
}

func (s *SubscriptionService) Update(ctx context.Context, subscription *models.Subscription) error {
	return s.subscriptionRepo.UpdateAt(ctx, subscription, s.now())
}

// ReplaceForTierChange atomically swaps oldSub (pre-mutated by the caller to
// its canceled state) for newSub in one transaction, so the one-live-
// subscription-per-(subject, tier-group) unique index is never violated and a
// failure leaves the old subscription active.
func (s *SubscriptionService) ReplaceForTierChange(ctx context.Context, oldSub, newSub *models.Subscription) error {
	return s.subscriptionRepo.ReplaceForTierChange(ctx, oldSub, newSub, s.now())
}

func (f GetSubscriptionsFilters) repo() SubscriptionFilters {
	var customer string
	if !f.CustomerID.IsZero() {
		customer = f.CustomerID.String()
	}
	return SubscriptionFilters{
		UserID: customer, Status: f.Status, PriceID: f.PriceID.UUID(), Rail: f.Rail,
		CreatedAfter: f.CreatedAfter, CreatedBefore: f.CreatedBefore,
		CanceledAfter: f.CanceledAfter, CanceledBefore: f.CanceledBefore, ExpiresBefore: f.ExpiresBefore,
		Dunning: f.Dunning,
	}
}

// ListSubscribers is one page of the merchant's subscriptions matching f,
// newest first.
func (s *SubscriptionService) ListSubscribers(ctx context.Context, f GetSubscriptionsFilters, page billing.PageRequest) (billing.ListPage[*models.Subscription], error) {
	if f.IDs != nil {
		rows, err := s.subscriptionRepo.ListByIDs(ctx, f.IDs)
		return billing.ListPage[*models.Subscription]{Items: rows}, err
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		return billing.ListPage[*models.Subscription]{}, err
	}
	afterAt, afterID, err := pagination.After(page.Cursor)
	if err != nil {
		return billing.ListPage[*models.Subscription]{}, err
	}
	rows, err := s.subscriptionRepo.ListPage(ctx, f.repo(), pagination.Fetch(limit), afterAt, afterID)
	if err != nil {
		return billing.ListPage[*models.Subscription]{}, err
	}
	return pagination.Cut(rows, limit, func(sub *models.Subscription) any {
		return pagination.TimeID{At: sub.CreatedAt, ID: sub.ID}
	}), nil
}

// CountSubscribers is how many of the merchant's subscriptions match f.
func (s *SubscriptionService) CountSubscribers(ctx context.Context, f GetSubscriptionsFilters) (int64, error) {
	return s.subscriptionRepo.Count(ctx, f.repo())
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

// GetActiveSubscription retrieves the active subscription for a user
func (s *SubscriptionService) GetActiveSubscription(ctx context.Context, userID string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetActiveSubscriptionAt(ctx, userID, s.now())
}

// GetByPSPSubscriptionID finds a subscription by rail and rail_subscription_id.
func (s *SubscriptionService) GetByPSPSubscriptionID(ctx context.Context, rail, railSubscriptionID string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetByPSPSubscriptionID(ctx, rail, railSubscriptionID)
}

func (s *SubscriptionService) GetByGatewayOrder(ctx context.Context, rail, orderID string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetByGatewayOrder(ctx, rail, orderID)
}

// GetActiveSubscriptionsForPSP gets all active subscriptions for a rail
func (s *SubscriptionService) GetActiveSubscriptionsForPSP(ctx context.Context, rail string) ([]*models.Subscription, error) {
	return s.subscriptionRepo.GetActiveSubscriptionsForPSP(ctx, rail)
}
