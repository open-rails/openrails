package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	repo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/platform"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/metric"
)

// SubscriptionLifecycleService handles the complete lifecycle of subscriptions
// including membership creation, renewal, cancellation, and expiration
type SubscriptionLifecycleService struct {
	DB                  *db.DB
	Config              *config.Config
	clock               clockwork.Clock
	ProductService      *catalog.ProductService
	PriceService        *catalog.PriceService
	EntitlementService  *entitlements.EntitlementService
	NotificationService NotificationEmailSender
	PaymentService      *payments.PaymentService // For creating Payment records on renewal
	EventLogService     LifecycleEventLogger     // For logging events to ClickHouse
	latency             metric.Float64Histogram
	errCounter          metric.Int64Counter
}

func (s *SubscriptionLifecycleService) assertActiveTransitionAllowed(ctx context.Context, subscription *models.Subscription, trigger string, allowOverride bool) error {
	reason, terminal := TerminalCancelReason(subscription)
	if !terminal {
		return nil
	}

	if allowOverride {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"processor":       subscription.Processor,
			"trigger":         trigger,
			"reason":          reason,
		}).Warn("Bypassing terminal transition guard via explicit manual override")
		return nil
	}

	return &TerminalTransitionBlockedError{
		SubscriptionID: subscription.ID,
		Processor:      subscription.Processor,
		FromStatus:     subscription.Status,
		ToStatus:       models.StatusActive,
		CancelType:     NormalizeCancelType(subscription.CancelType),
		Trigger:        trigger,
		Reason:         reason,
	}
}

// NewSubscriptionLifecycleService creates a new instance of SubscriptionLifecycleService
func NewSubscriptionLifecycleService(db *db.DB, productService *catalog.ProductService, priceService *catalog.PriceService, entitlementService *entitlements.EntitlementService, notificationService NotificationEmailSender, paymentService *payments.PaymentService, eventLogService LifecycleEventLogger, clocks ...clockwork.Clock) *SubscriptionLifecycleService {
	meter, _ := platform.InitTelemetry()
	latency, _ := meter.Float64Histogram("subscription_create_latency_seconds", metric.WithDescription("Latency of membership creation"))
	errCounter, _ := meter.Int64Counter("subscription_create_errors_total", metric.WithDescription("Total count of subscription creation errors"))
	return &SubscriptionLifecycleService{
		DB:                  db,
		Config:              nil,                            // Set via SetConfig if feature flags are needed
		clock:               timeutil.FirstClock(clocks...), // Default to real clock, can be overridden for tests
		ProductService:      productService,
		PriceService:        priceService,
		EntitlementService:  entitlementService,
		NotificationService: notificationService,
		PaymentService:      paymentService,
		EventLogService:     eventLogService,
		latency:             latency,
		errCounter:          errCounter,
	}
}

// SetClock allows replacing the clock for testing
func (s *SubscriptionLifecycleService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *SubscriptionLifecycleService) Clock() clockwork.Clock {
	return s.clock
}

// SetConfig sets the config for feature flag access
func (s *SubscriptionLifecycleService) SetConfig(cfg *config.Config) {
	s.Config = cfg
}

// now returns the current time from the service's clock
func (s *SubscriptionLifecycleService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *SubscriptionLifecycleService) dispatchNotifications(ctx context.Context, notifications []*models.NotificationQueue) {
	if s.NotificationService == nil {
		return
	}
	for _, notification := range notifications {
		if err := s.NotificationService.DeliverEmail(ctx, notification); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"notification_id": notification.ID,
				"event_type":      notification.EventType,
				"user_id":         notification.TenantSubjectID.String(),
			}).Error("failed to deliver notification email")
		}
	}
}

// CreateMembership creates a new subscription and grants associated roles
func (s *SubscriptionLifecycleService) CreateMembership(ctx context.Context, params *CreateMembershipParams) (*models.Subscription, error) {
	start := time.Now()
	defer func() {
		s.latency.Record(ctx, time.Since(start).Seconds())
	}()

	var (
		subscription  *models.Subscription
		notifications []*models.NotificationQueue
	)

	procSubID := normalize.FromPtr(params.ProcessorSubscriptionID)

	log.WithContext(ctx).WithFields(log.Fields{
		"user_id":                   params.UserID,
		"price_id":                  params.PriceID,
		"processor":                 params.Processor,
		"processor_subscription_id": procSubID,
		"transaction_id":            params.TransactionID,
		"amount_cents":              params.Amount,
		"currency":                  params.Currency,
	}).Info("Starting membership creation flow")

	err := s.DB.RunInTenantTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		dbb := db.NewWithTx(tx)
		var err error
		subscription, notifications, err = s.createMembershipCore(ctx, dbb, params)
		return err
	})
	if err != nil {
		s.errCounter.Add(ctx, 1)
		return nil, err
	}

	s.dispatchNotifications(ctx, notifications)

	// Log the charge success event to ClickHouse
	s.logPaymentEvent(ctx, subscription, params.Processor, params.TransactionID, params.Amount, params.Currency)

	return subscription, nil
}

// CreateMembershipTx executes the membership creation logic using the provided transactional DB.
// The caller is responsible for wrapping the call in a transaction and dispatching any queued notifications.
func (s *SubscriptionLifecycleService) CreateMembershipTx(ctx context.Context, txDB *db.DB, params *CreateMembershipParams) (*models.Subscription, []*models.NotificationQueue, error) {
	if txDB == nil {
		return nil, nil, errors.New("transaction DB is required")
	}
	return s.createMembershipCore(ctx, txDB, params)
}

func (s *SubscriptionLifecycleService) createMembershipCore(ctx context.Context, dbb *db.DB, params *CreateMembershipParams) (*models.Subscription, []*models.NotificationQueue, error) {
	if dbb == nil {
		return nil, nil, errors.New("database handle is required")
	}

	priceService := catalog.NewPriceService(dbb)
	productService := catalog.NewProductService(dbb)
	entitlementService := entitlements.NewEntitlementService(dbb, s.Clock())
	entitlementService.SetClock(s.Clock()) // Propagate clock for testing
	notificationRepo := repo.NewNotificationQueueRepo(dbb)
	subService := NewSubscriptionService(dbb, priceService, productService, nil, nil, s.Clock())

	price, err := priceService.GetByID(ctx, params.PriceID)
	if err != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"user_id":  params.UserID,
			"price_id": params.PriceID,
		}).WithError(err).Error("Failed to load price for membership creation")
		return nil, nil, fmt.Errorf("failed to get price: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"user_id":    params.UserID,
		"price_id":   price.ID,
		"product_id": price.ProductID,
	}).Info("Loaded price for membership creation")

	var existingPendingSub *models.Subscription
	if params.ProcessorSubscriptionID != nil && strings.TrimSpace(*params.ProcessorSubscriptionID) != "" {
		found, err := subService.GetByProcessorSubscriptionID(ctx, string(params.Processor), strings.TrimSpace(*params.ProcessorSubscriptionID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("failed to check existing subscription by processor subscription ID: %w", err)
		}
		if err == nil && found.Status == models.StatusPending {
			if found.TenantSubjectID.String() != params.UserID || found.ProductID != price.ProductID {
				return nil, nil, fmt.Errorf("processor subscription belongs to a different pending subscription")
			}
			existingPendingSub = found
		}
	}

	var paymentService *payments.PaymentService
	if params.TransactionID != "" && s.PaymentService != nil {
		paymentService = payments.NewPaymentService(dbb, s.Clock())
		existingPayment, err := paymentService.GetByTransactionID(ctx, params.Processor, params.TransactionID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("failed to check existing payment: %w", err)
		}
		if err == nil {
			if existingPayment.TenantSubjectID.String() != params.UserID {
				return nil, nil, fmt.Errorf("payment transaction belongs to a different user")
			}
			if existingPayment.PriceID != price.ID {
				return nil, nil, fmt.Errorf("payment transaction belongs to a different price")
			}
			if existingPayment.SubscriptionID == nil {
				return nil, nil, fmt.Errorf("payment transaction is not linked to a subscription")
			}
			expectedAmount := params.Amount
			if !params.AmountProvided && expectedAmount == 0 {
				expectedAmount = price.Amount
			}
			expectedCurrency := strings.TrimSpace(params.Currency)
			if expectedCurrency == "" {
				expectedCurrency = price.Currency
			}
			if err := validateCompletedPayment(existingPayment, expectedAmount, expectedCurrency); err != nil {
				return nil, nil, err
			}
			existingSubscription, err := subService.GetByID(ctx, *existingPayment.SubscriptionID)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to load subscription for duplicate payment transaction: %w", err)
			}
			if existingSubscription.TenantSubjectID.String() != params.UserID {
				return nil, nil, fmt.Errorf("payment transaction subscription belongs to a different user")
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": existingSubscription.ID,
				"user_id":         params.UserID,
				"payment_id":      existingPayment.ID,
				"transaction_id":  params.TransactionID,
			}).Info("Membership payment already exists; skipping duplicate membership creation")
			return existingSubscription, nil, nil
		}
	}

	activeSub, err := subService.GetActiveOrPendingByUserIDAndProductID(ctx, params.UserID, price.ProductID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("failed to check existing subscriptions: %w", err)
	}

	if err == nil && (existingPendingSub == nil || activeSub.ID != existingPendingSub.ID) {
		log.WithContext(ctx).WithFields(log.Fields{
			"user_id":                   params.UserID,
			"product_id":                price.ProductID,
			"existing_subscription_id":  activeSub.ID,
			"existing_price_id":         activeSub.PriceID,
			"existing_status":           activeSub.Status,
			"existing_processor":        activeSub.Processor,
			"processor_subscription_id": activeSub.ProcessorSubscriptionID,
		}).Warn("User already has an active, pending, or past-due subscription for this product; aborting membership creation")
		return nil, nil, fmt.Errorf("user already has an active, pending, or past-due subscription for this product")
	}

	now := s.now()
	periodStartsAt := now
	if params.CurrentPeriodStartsAt != nil && !params.CurrentPeriodStartsAt.IsZero() {
		periodStartsAt = params.CurrentPeriodStartsAt.UTC()
	}
	var periodEndsAt time.Time
	if params.CurrentPeriodEndsAt != nil && !params.CurrentPeriodEndsAt.IsZero() && params.CurrentPeriodEndsAt.After(periodStartsAt) {
		periodEndsAt = params.CurrentPeriodEndsAt.UTC()
	} else if price.BillingCycleDays != nil && *price.BillingCycleDays > 0 {
		periodEndsAt = now.Add(time.Duration(*price.BillingCycleDays) * 24 * time.Hour)
	} else {
		periodEndsAt = now.Add(30 * 24 * time.Hour)
	}
	product, err := productService.GetByID(ctx, price.ProductID)
	if err != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"product_id": price.ProductID,
			"user_id":    params.UserID,
		}).WithError(err).Error("Failed to load product for membership creation")
		return nil, nil, fmt.Errorf("failed to get product: %w", err)
	}

	var subscription *models.Subscription
	if existingPendingSub != nil {
		if params.UserEmail != nil && strings.TrimSpace(*params.UserEmail) != "" {
			emailc := strings.TrimSpace(*params.UserEmail)
			existingPendingSub.UserEmail = &emailc
		}

		existingPendingSub.PriceID = price.ID
		existingPendingSub.ProductID = price.ProductID
		existingPendingSub.Status = models.StatusActive
		existingPendingSub.Processor = params.Processor
		if params.ProcessorSubscriptionID != nil {
			existingPendingSub.ProcessorSubscriptionID = *params.ProcessorSubscriptionID
		}

		existingPendingSub.CurrentPeriodStartsAt = &periodStartsAt
		existingPendingSub.CurrentPeriodEndsAt = &periodEndsAt
		if len(existingPendingSub.EntitlementsSpecSnapshot) == 0 {
			existingPendingSub.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(product.EntitlementsSpec)
		}
		if len(existingPendingSub.CreditsSpecSnapshot) == 0 {
			existingPendingSub.CreditsSpecSnapshot = models.CloneCreditsSpec(product.CreditsSpec)
		}
		existingPendingSub.StartedAt = periodStartsAt
		existingPendingSub.CancelledAt = nil
		existingPendingSub.CancelType = nil
		existingPendingSub.EndedAt = nil

		if err := subService.Update(ctx, existingPendingSub); err != nil {
			return nil, nil, fmt.Errorf("failed to update subscription: %w", err)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":           existingPendingSub.ID,
			"user_id":                   existingPendingSub.TenantSubjectID.String(),
			"price_id":                  existingPendingSub.PriceID,
			"processor":                 existingPendingSub.Processor,
			"processor_subscription_id": existingPendingSub.ProcessorSubscriptionID,
			"period_start":              periodStartsAt,
			"period_end":                periodEndsAt,
		}).Info("Activating existing pending subscription record for membership creation")
		subscription = existingPendingSub
	} else {
		subscription = &models.Subscription{
			ID:                       uuidutil.NewV7(),
			TenantSubjectID:          identity.TenantSubjectIDFromString(params.UserID).UUID(),
			ProductID:                price.ProductID,
			PriceID:                  price.ID,
			EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(product.EntitlementsSpec),
			CreditsSpecSnapshot:      models.CloneCreditsSpec(product.CreditsSpec),
			Status:                   models.StatusActive,
			Processor:                params.Processor,
			ProcessorSubscriptionID: func() string {
				if params.ProcessorSubscriptionID != nil {
					return *params.ProcessorSubscriptionID
				}
				return ""
			}(),
			CurrentPeriodStartsAt: &periodStartsAt,
			CurrentPeriodEndsAt:   &periodEndsAt,
			StartedAt:             periodStartsAt,
		}

		if params.UserEmail != nil && strings.TrimSpace(*params.UserEmail) != "" {
			emailc := strings.TrimSpace(*params.UserEmail)
			subscription.UserEmail = &emailc
		}

		if err := subService.Create(ctx, subscription); err != nil {
			return nil, nil, fmt.Errorf("failed to create subscription: %w", err)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":           subscription.ID,
			"user_id":                   subscription.TenantSubjectID.String(),
			"price_id":                  subscription.PriceID,
			"processor":                 subscription.Processor,
			"processor_subscription_id": subscription.ProcessorSubscriptionID,
			"period_start":              periodStartsAt,
			"period_end":                periodEndsAt,
		}).Info("Created new subscription record for membership creation")
	}

	notifications := make([]*models.NotificationQueue, 0, 1)

	if entitlementService != nil {
		entNames := make([]string, 0, 4)
		entitlementsSpec := subscription.EntitlementsSpecSnapshot
		if len(entitlementsSpec) == 0 {
			entitlementsSpec = product.EntitlementsSpec
		}
		if len(entitlementsSpec) > 0 {
			for name := range entitlementsSpec {
				entNames = append(entNames, name)
			}
		} else {
			entNames = append(entNames, "premium")
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"user_id":         subscription.TenantSubjectID.String(),
			"entitlements":    entNames,
		}).Info("Preparing to grant subscription entitlements")

		for _, ent := range entNames {
			existsBySource, err := entitlementService.ExistsBySource(ctx, models.EntitlementSourceSubscription, subscription.ID, ent)
			if err != nil {
				return nil, nil, fmt.Errorf("failed entitlement check: %w", err)
			}
			if existsBySource {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"user_id":         subscription.TenantSubjectID.String(),
					"entitlement":     ent,
				}).Info("Entitlement already granted for subscription; skipping")
				continue
			}

			notBefore := periodStartsAt.UTC()
			endAt := periodEndsAt.UTC()
			window, err := entitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
				UserID:      subscription.TenantSubjectID.String(),
				Entitlement: ent,
				NotBefore:   &notBefore,
				EndAt:       &endAt,
				SourceType:  models.EntitlementSourceSubscription,
				SourceID:    subscription.ID,
			})
			if err != nil {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"user_id":         subscription.TenantSubjectID.String(),
					"entitlement":     ent,
				}).WithError(err).Error("Failed to grant subscription entitlement")
				return nil, nil, fmt.Errorf("failed to grant entitlement %s: %w", ent, err)
			}
			if window == nil {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"user_id":         subscription.TenantSubjectID.String(),
					"entitlement":     ent,
					"window_start":    window.StartAt,
					"window_end":      window.EndAt,
				}).Info("Subscription entitlement already covered by canonical timeline")
				continue
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.TenantSubjectID.String(),
				"entitlement":     ent,
				"window_start":    window.StartAt,
				"window_end":      window.EndAt,
			}).Info("Granted subscription entitlement")
		}
	}

	notification := &models.NotificationQueue{
		ID:              uuidutil.NewV7(),
		TenantSubjectID: subscription.TenantSubjectID,
		EventType:       models.NotificationPremiumStarted,
	}
	if err := notificationRepo.Create(ctx, notification); err != nil {
		log.WithContext(ctx).WithError(err).Error("failed to create membership started notification")
	} else {
		notifications = append(notifications, notification)
	}

	// Create Payment record if payment info is provided
	if params.TransactionID != "" && s.PaymentService != nil {
		paymentService := payments.NewPaymentService(dbb, s.Clock())
		payment := &models.Payment{
			ID:              uuidutil.NewV7(),
			TenantSubjectID: subscription.TenantSubjectID,
			SubscriptionID:  &subscription.ID,
			PriceID:         price.ID,
			Processor:       params.Processor,
			TransactionID:   params.TransactionID,
			Amount:          params.Amount,
			Currency:        params.Currency,
			Status:          payments.PaymentStatusCompletedValue,
			PurchasedAt:     now,
			CreatedAt:       now,
		}
		if err := paymentService.Create(ctx, payment); err != nil {
			log.WithContext(ctx).WithError(err).Error("failed to create payment record for membership")
		}
	}

	return subscription, notifications, nil
}

func (s *SubscriptionLifecycleService) logPaymentEvent(ctx context.Context, subscription *models.Subscription, processor models.Processor, transactionID string, amount int64, currency string) {
	if s.EventLogService != nil {
		s.EventLogService.LogEvent(ctx, LifecycleEvent{
			TenantSubjectID: subscription.TenantSubjectID.String(),
			SubscriptionID:  subscription.ID.String(),
			EventType:       "membership_created",
			Processor:       string(processor),
			TransactionID:   transactionID,
			Amount:          amount,
			Currency:        currency,
			Timestamp:       time.Now(),
		})
	}
}

func validateCompletedPayment(payment *models.Payment, expectedAmount int64, expectedCurrency string) error {
	if !payments.PaymentStatusCompleted(payment.Status) {
		return fmt.Errorf("payment transaction is not completed")
	}
	if expectedAmount > 0 && payment.Amount != expectedAmount {
		return fmt.Errorf("payment transaction amount mismatch (expected %d, got %d)", expectedAmount, payment.Amount)
	}
	if expectedCurrency != "" && payment.Currency != expectedCurrency {
		return fmt.Errorf("payment transaction currency mismatch (expected %s, got %s)", expectedCurrency, payment.Currency)
	}
	return nil
}
