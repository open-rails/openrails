package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	"github.com/open-rails/openrails/internal/shared/apperr"
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
	// Sort is the list's order; empty is created_at.
	Sort billing.SubscriptionSort `form:"sort"`
	// IDs, when not nil, reads those subscriptions instead, in one page.
	IDs []uuid.UUID `form:"-"`
}

type SubscriptionService struct {
	db                       *db.DB
	subscriptionRepo         *SubscriptionRepo
	notificationRepo         *NotificationService
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
		notificationRepo:     NewNotificationService(db),
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

func (s *SubscriptionService) filters(ctx context.Context, f GetSubscriptionsFilters) (SubscriptionFilters, error) {
	var priceID uuid.UUID
	if !f.PriceID.IsZero() {
		price, err := catalog.NewPriceService(s.Database()).GetByPublicID(ctx, f.PriceID)
		if err != nil {
			return SubscriptionFilters{}, err
		}
		priceID = price.ID
	}
	var customer string
	if !f.CustomerID.IsZero() {
		customer = f.CustomerID.String()
	}
	return SubscriptionFilters{
		UserID: customer, Status: f.Status, PriceID: priceID, Rail: f.Rail,
		CreatedAfter: f.CreatedAfter, CreatedBefore: f.CreatedBefore,
		CanceledAfter: f.CanceledAfter, CanceledBefore: f.CanceledBefore, ExpiresBefore: f.ExpiresBefore,
		Dunning: f.Dunning, Sort: f.Sort,
	}, nil
}

// ListSubscribers is one page of the merchant's subscriptions matching f, in
// f.Sort's order. A cursor of another order is refused.
func (s *SubscriptionService) ListSubscribers(ctx context.Context, f GetSubscriptionsFilters, page billing.PageRequest) (billing.ListPage[*models.Subscription], error) {
	sortKey := subscriptionSortKey(f.Sort)
	if sortKey == nil && f.Sort != "" && f.Sort != billing.SubscriptionSortCreatedAt {
		return billing.ListPage[*models.Subscription]{}, ErrInvalidSubscriptionSort
	}
	if f.IDs != nil {
		rows, err := s.subscriptionRepo.ListByIDs(ctx, f.IDs)
		return billing.ListPage[*models.Subscription]{Items: rows}, err
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		return billing.ListPage[*models.Subscription]{}, err
	}
	position := func(sub *models.Subscription) any { return pagination.TimeID{At: sub.CreatedAt, ID: sub.ID} }
	var afterAt *time.Time
	var afterID *uuid.UUID
	switch f.Sort {
	case "", billing.SubscriptionSortCreatedAt:
		afterAt, afterID, err = pagination.After(page.Cursor)
	default:
		afterAt, afterID, err = pagination.AfterSorted(page.Cursor, string(f.Sort))
		position = func(sub *models.Subscription) any {
			return pagination.SortedTimeID{Sort: string(f.Sort), At: *sortKey(sub), ID: sub.ID}
		}
	}
	if err != nil {
		return billing.ListPage[*models.Subscription]{}, err
	}
	filter, err := s.filters(ctx, f)
	if db.IsNotFound(err) {
		return billing.ListPage[*models.Subscription]{Items: []*models.Subscription{}}, nil
	}
	if err != nil {
		return billing.ListPage[*models.Subscription]{}, err
	}
	rows, err := s.subscriptionRepo.ListPage(ctx, filter, pagination.Fetch(limit), afterAt, afterID)
	if err != nil {
		return billing.ListPage[*models.Subscription]{}, err
	}
	return pagination.Cut(rows, limit, position), nil
}

// ErrInvalidSubscriptionSort is a sort the subscription list does not offer.
var ErrInvalidSubscriptionSort = apperr.New(http.StatusBadRequest, billing.CodeInvalidQuery, "sort is invalid").WithParam("sort")

// subscriptionSortKey is the time a non-default order sorts by; nil for
// created_at or an unknown order. Its rows always carry it.
func subscriptionSortKey(sort billing.SubscriptionSort) func(*models.Subscription) *time.Time {
	switch sort {
	case billing.SubscriptionSortCurrentPeriodEndsAt:
		return func(sub *models.Subscription) *time.Time { return sub.CurrentPeriodEndsAt }
	case billing.SubscriptionSortCanceledAt:
		return func(sub *models.Subscription) *time.Time { return sub.CanceledAt }
	}
	return nil
}

// CountSubscribers is how many of the merchant's subscriptions match f.
func (s *SubscriptionService) CountSubscribers(ctx context.Context, f GetSubscriptionsFilters) (int64, error) {
	filter, err := s.filters(ctx, f)
	if db.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return s.subscriptionRepo.Count(ctx, filter)
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
