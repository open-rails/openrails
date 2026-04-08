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
	"github.com/open-rails/openrails/internal/shared/normalize"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
)

// SubscriptionLifecycleService handles the complete lifecycle of subscriptions
// including membership creation, renewal, cancellation, and expiration
type SubscriptionLifecycleService struct {
	DB                  *db.DB
	Config              *config.Config
	Clock               clockwork.Clock
	ProductService      *catalog.ProductService
	PriceService        *catalog.PriceService
	EntitlementService  *entitlements.EntitlementService
	NotificationService NotificationEmailSender
	PaymentService      *payments.PaymentService // For creating Payment records on renewal
	EventLogService     LifecycleEventLogger     // For logging events to ClickHouse
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
func NewSubscriptionLifecycleService(db *db.DB, productService *catalog.ProductService, priceService *catalog.PriceService, entitlementService *entitlements.EntitlementService, notificationService NotificationEmailSender, paymentService *payments.PaymentService, eventLogService LifecycleEventLogger) *SubscriptionLifecycleService {
	return &SubscriptionLifecycleService{
		DB:                  db,
		Config:              nil,                      // Set via SetConfig if feature flags are needed
		Clock:               clockwork.NewRealClock(), // Default to real clock, can be overridden for tests
		ProductService:      productService,
		PriceService:        priceService,
		EntitlementService:  entitlementService,
		NotificationService: notificationService,
		PaymentService:      paymentService,
		EventLogService:     eventLogService,
	}
}

// SetClock allows replacing the clock for testing
func (s *SubscriptionLifecycleService) SetClock(c clockwork.Clock) {
	s.Clock = c
}

// SetConfig sets the config for feature flag access
func (s *SubscriptionLifecycleService) SetConfig(cfg *config.Config) {
	s.Config = cfg
}

// now returns the current time from the service's clock
func (s *SubscriptionLifecycleService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
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
				"user_id":         notification.UserID,
			}).Error("failed to deliver notification email")
		}
	}
}

// CreateMembership creates a new subscription and grants associated roles
func (s *SubscriptionLifecycleService) CreateMembership(ctx context.Context, params *CreateMembershipParams) (*models.Subscription, error) {
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

	err := s.DB.GetDB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		dbb := db.NewWithTx(tx)
		var err error
		subscription, notifications, err = s.createMembershipCore(ctx, dbb, params)
		return err
	})
	if err != nil {
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
	entitlementService := entitlements.NewEntitlementService(dbb)
	entitlementService.SetClock(s.Clock) // Propagate clock for testing
	notificationRepo := repo.NewNotificationQueueRepo(dbb)
	subService := NewSubscriptionService(dbb, priceService, productService, nil, nil, nil)

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

	activeSub, err := subService.GetActiveOrPendingByUserIDAndProductID(ctx, params.UserID, price.ProductID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("failed to check existing subscriptions: %w", err)
	}

	// Only stop if an active subscription exists
	if err == nil && activeSub.Status == models.StatusActive {
		log.WithContext(ctx).WithFields(log.Fields{
			"user_id":                   params.UserID,
			"product_id":                price.ProductID,
			"existing_subscription_id":  activeSub.ID,
			"existing_price_id":         activeSub.PriceID,
			"existing_status":           activeSub.Status,
			"existing_processor":        activeSub.Processor,
			"processor_subscription_id": activeSub.ProcessorSubscriptionID,
		}).Warn("User already has an active or pending subscription for this product; aborting membership creation")
		return nil, nil, fmt.Errorf("user already has an active or pending subscription for this product")
	}

	existingSub, err := subService.GetByUserIDAndPriceID(ctx, params.UserID, price.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("failed to check existing subscription by price: %w", err)
	}
	if err != nil {
		existingSub = nil
	}

	now := s.now()
	periodStartsAt := now
	var periodEndsAt time.Time
	if params.CurrentPeriodEndsAt != nil && !params.CurrentPeriodEndsAt.IsZero() && params.CurrentPeriodEndsAt.After(periodStartsAt) {
		periodEndsAt = params.CurrentPeriodEndsAt.UTC()
	} else if price.BillingCycleDays != nil && *price.BillingCycleDays > 0 {
		periodEndsAt = now.Add(time.Duration(*price.BillingCycleDays) * 24 * time.Hour)
	} else {
		periodEndsAt = now.Add(30 * 24 * time.Hour)
	}

	var subscription *models.Subscription
	if existingSub != nil {
		if params.UserEmail != nil && strings.TrimSpace(*params.UserEmail) != "" {
			emailc := strings.TrimSpace(*params.UserEmail)
			existingSub.UserEmail = &emailc
		}

		existingSub.PriceID = price.ID
		existingSub.Status = models.StatusActive
		existingSub.Processor = params.Processor
		if params.ProcessorSubscriptionID != nil {
			existingSub.ProcessorSubscriptionID = *params.ProcessorSubscriptionID
		}

		existingSub.CurrentPeriodStartsAt = &periodStartsAt
		existingSub.CurrentPeriodEndsAt = &periodEndsAt
		existingSub.StartedAt = periodStartsAt
		existingSub.CancelledAt = nil
		existingSub.CancelType = nil
		existingSub.CancelFeedback = nil
		existingSub.EndedAt = nil

		if err := subService.Update(ctx, existingSub); err != nil {
			return nil, nil, fmt.Errorf("failed to update subscription: %w", err)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":           existingSub.ID,
			"user_id":                   existingSub.UserID,
			"price_id":                  existingSub.PriceID,
			"processor":                 existingSub.Processor,
			"processor_subscription_id": existingSub.ProcessorSubscriptionID,
			"period_start":              periodStartsAt,
			"period_end":                periodEndsAt,
		}).Info("Reusing existing subscription record for membership creation")
		subscription = existingSub
	} else {
		subscription = &models.Subscription{
			ID:        uuid.New(),
			UserID:    params.UserID,
			ProductID: price.ProductID,
			PriceID:   price.ID,
			Status:    models.StatusActive,
			Processor: params.Processor,
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
			"user_id":                   subscription.UserID,
			"price_id":                  subscription.PriceID,
			"processor":                 subscription.Processor,
			"processor_subscription_id": subscription.ProcessorSubscriptionID,
			"period_start":              periodStartsAt,
			"period_end":                periodEndsAt,
		}).Info("Created new subscription record for membership")
	}

	notifications := make([]*models.NotificationQueue, 0, 1)

	if entitlementService != nil {
		product, err := productService.GetByID(ctx, price.ProductID)
		if err != nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"product_id": price.ProductID,
				"user_id":    params.UserID,
			}).WithError(err).Error("Failed to load product for entitlement grant")
			return nil, nil, fmt.Errorf("failed to get product: %w", err)
		}

		entNames := make([]string, 0, 4)
		if len(product.EntitlementsSpec) > 0 {
			for name := range product.EntitlementsSpec {
				entNames = append(entNames, name)
			}
		} else {
			entNames = append(entNames, "premium")
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"user_id":         subscription.UserID,
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
					"user_id":         subscription.UserID,
					"entitlement":     ent,
				}).Info("Entitlement already granted for subscription; skipping")
				continue
			}

			notBefore := periodStartsAt.UTC()
			endAt := periodEndsAt.UTC()
			window, err := entitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
				UserID:      params.UserID,
				Entitlement: ent,
				NotBefore:   &notBefore,
				EndAt:       &endAt,
				SourceType:  models.EntitlementSourceSubscription,
				SourceID:    subscription.ID,
			})
			if err != nil {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"user_id":         subscription.UserID,
					"entitlement":     ent,
				}).WithError(err).Error("Failed to grant subscription entitlement")
				return nil, nil, fmt.Errorf("failed to grant entitlement %s: %w", ent, err)
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.UserID,
				"entitlement":     ent,
				"window_start":    window.StartAt,
				"window_end":      window.EndAt,
			}).Info("Granted subscription entitlement")
		}
	}

	notification := &models.NotificationQueue{
		ID:        uuid.New(),
		UserID:    params.UserID,
		EventType: models.NotificationPremiumStarted,
	}
	if err := notificationRepo.Create(ctx, notification); err != nil {
		log.WithContext(ctx).WithError(err).Error("failed to create membership started notification")
	} else {
		notifications = append(notifications, notification)
	}

	// Create Payment record if payment info is provided
	if params.TransactionID != "" && s.PaymentService != nil {
		paymentService := payments.NewPaymentService(dbb)
		existingPayment, err := paymentService.GetByTransactionID(ctx, params.Processor, params.TransactionID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("failed to check existing payment: %w", err)
		}
		if err == nil && existingPayment.UserID == subscription.UserID {
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.UserID,
				"payment_id":      existingPayment.ID,
				"transaction_id":  params.TransactionID,
			}).Info("Payment already exists for transaction; skipping")
			return subscription, notifications, nil
		}

		// Use provided amount/currency or fall back to price defaults
		amount := params.Amount
		if amount == 0 {
			amount = price.Amount
		}
		currency := params.Currency
		if currency == "" {
			currency = price.Currency
		}

		payment := &models.Payment{
			ID:             uuid.New(),
			UserID:         params.UserID,
			PriceID:        price.ID,
			SubscriptionID: &subscription.ID,
			Processor:      params.Processor,
			TransactionID:  params.TransactionID,
			Amount:         amount,
			ListAmount:     price.Amount,
			Currency:       currency,
			Metadata:       params.PaymentMetadata,
			PurchasedAt:    now,
			CreatedAt:      now,
		}
		if err := paymentService.Create(ctx, payment); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"transaction_id":  params.TransactionID,
				"subscription_id": subscription.ID,
				"user_id":         params.UserID,
			}).Error("failed to create payment record for new membership")
			// Don't fail the membership creation - just log the error
		} else {
			log.WithContext(ctx).WithFields(log.Fields{
				"transaction_id":  params.TransactionID,
				"subscription_id": subscription.ID,
				"user_id":         params.UserID,
				"amount_cents":    amount,
				"currency":        currency,
			}).Info("Recorded payment for membership creation")
		}
	}

	return subscription, notifications, nil
}

// RenewMembership renews an existing subscription and extends the membership.
// It also creates a Payment record for the renewal transaction.
// If a scheduled downgrade exists (ScheduledPriceID), it will be applied on renewal.
func (s *SubscriptionLifecycleService) RenewMembership(ctx context.Context, params *RenewMembershipParams) error {
	notifications := make([]*models.NotificationQueue, 0, 1)
	renewalApplied := false

	// Variables to capture from transaction for logging/notifications.
	var subscriptionID uuid.UUID
	var userID string

	log.WithContext(ctx).WithFields(log.Fields{
		"processor":                 params.Processor,
		"processor_subscription_id": params.ProcessorSubscriptionID,
		"transaction_id":            params.TransactionID,
		"amount_cents":              params.Amount,
		"currency":                  params.Currency,
		"allow_terminal_reactivate": params.AllowTerminalReactivation,
	}).Info("Starting membership renewal flow")

	err := s.DB.GetDB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		db := db.NewWithTx(tx)
		priceService := catalog.NewPriceService(db)
		productService := catalog.NewProductService(db)
		notificationRepo := repo.NewNotificationQueueRepo(db)
		subService := NewSubscriptionService(db, priceService, productService, nil, nil, nil)
		entitlementService := entitlements.NewEntitlementService(db)
		paymentService := payments.NewPaymentService(db)
		entitlementService.SetClock(s.Clock)

		// Find subscription - use processor name for gateway lookup
		subscription, err := subService.GetByProcessorSubscriptionID(ctx, string(params.Processor), params.ProcessorSubscriptionID)
		if err != nil {
			// Try fallback: look up by SubscriptionID using ProcessorSubscriptionID as UUID
			log.WithContext(ctx).WithFields(log.Fields{
				"processor":                 params.Processor,
				"processor_subscription_id": params.ProcessorSubscriptionID,
			}).WithError(err).Warn("GetByProcessorSubscriptionID failed, attempting fallback by SubscriptionID")
			var subID uuid.UUID
			if uuidParsed, parseErr := uuid.Parse(params.ProcessorSubscriptionID); parseErr == nil {
				subID = uuidParsed
				subscription, err = subService.GetByID(ctx, subID)
			}
			if err != nil {
				log.WithContext(ctx).WithFields(log.Fields{
					"processor":                 params.Processor,
					"processor_subscription_id": params.ProcessorSubscriptionID,
				}).WithError(err).Error("Failed to load subscription for renewal (both lookups)")
				return fmt.Errorf("subscription not found: %w", err)
			}
		}

		if err := s.assertActiveTransitionAllowed(ctx, subscription, "renewal", params.AllowTerminalReactivation); err != nil {
			return err
		}

		// Capture values for Payment creation after transaction
		subscriptionID = subscription.ID
		userID = subscription.UserID

		// Check for scheduled downgrade
		var price *models.Price
		var oldProduct, newProduct *models.Product
		applyingDowngrade := subscription.ScheduledPriceID != nil

		if applyingDowngrade {
			// Get old product for entitlement comparison
			oldPrice, err := priceService.GetByID(ctx, subscription.PriceID)
			if err != nil {
				return fmt.Errorf("failed to get current price: %w", err)
			}
			oldProduct, err = productService.GetByID(ctx, oldPrice.ProductID)
			if err != nil {
				return fmt.Errorf("failed to get current product: %w", err)
			}

			// Apply the scheduled downgrade - switch to the new price
			price, err = priceService.GetByID(ctx, *subscription.ScheduledPriceID)
			if err != nil {
				return fmt.Errorf("failed to get scheduled price: %w", err)
			}

			newProduct, err = productService.GetByID(ctx, price.ProductID)
			if err != nil {
				return fmt.Errorf("failed to get new product: %w", err)
			}

			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.UserID,
				"old_price_id":    subscription.PriceID,
				"new_price_id":    price.ID,
				"old_product":     oldProduct.DisplayName,
				"new_product":     newProduct.DisplayName,
			}).Info("Applying scheduled downgrade on renewal")

			// Update subscription to new price and product
			subscription.PriceID = price.ID
			subscription.ProductID = price.ProductID
			subscription.ScheduledPriceID = nil // Clear the scheduled downgrade
		} else {
			// Normal renewal - use current price
			price, err = priceService.GetByID(ctx, subscription.PriceID)
			if err != nil {
				return fmt.Errorf("failed to get price: %w", err)
			}
		}

		amount := params.Amount
		if amount <= 0 {
			amount = price.Amount
		}
		currency := strings.TrimSpace(params.Currency)
		if currency == "" {
			currency = price.Currency
		}

		if params.TransactionID != "" {
			now := s.now().UTC()
			payment := &models.Payment{
				ID:             uuid.New(),
				UserID:         subscription.UserID,
				PriceID:        price.ID,
				SubscriptionID: &subscription.ID,
				Processor:      params.Processor,
				TransactionID:  params.TransactionID,
				Amount:         amount,
				ListAmount:     amount,
				Currency:       currency,
				Metadata:       params.PaymentMetadata,
				PurchasedAt:    now,
				CreatedAt:      now,
			}
			created, err := paymentService.CreateIfNotExists(ctx, payment)
			if err != nil {
				return fmt.Errorf("failed to persist renewal payment marker: %w", err)
			}
			if !created {
				log.WithContext(ctx).WithFields(log.Fields{
					"transaction_id":  params.TransactionID,
					"subscription_id": subscription.ID,
					"processor":       params.Processor,
				}).Info("Renewal already processed; skipping duplicate lifecycle mutation")
				return nil
			}
		}

		// Calculate new billing period
		var periodStartsAt, periodEndsAt time.Time
		if subscription.CurrentPeriodEndsAt != nil && !subscription.CurrentPeriodEndsAt.IsZero() {
			periodStartsAt = *subscription.CurrentPeriodEndsAt
			periodEndsAt = periodStartsAt.Add(time.Duration(*price.BillingCycleDays) * 24 * time.Hour)
		} else {
			now := s.now()
			periodStartsAt = now
			periodEndsAt = now.Add(time.Duration(*price.BillingCycleDays) * 24 * time.Hour)
		}
		if params.CurrentPeriodEndsAt != nil && !params.CurrentPeriodEndsAt.IsZero() && params.CurrentPeriodEndsAt.After(periodStartsAt) {
			periodEndsAt = params.CurrentPeriodEndsAt.UTC()
		}

		// Update subscription
		subscription.Status = models.StatusActive
		// Transaction IDs now stored in Purchase table
		subscription.CurrentPeriodStartsAt = &periodStartsAt
		subscription.CurrentPeriodEndsAt = &periodEndsAt
		subscription.CancelledAt = nil
		subscription.CancelType = nil
		subscription.CancelFeedback = nil
		subscription.EndedAt = nil
		// Clear any dunning/grace fields on successful renewal.
		subscription.LastRetryAt = nil
		subscription.RetryAttempts = nil
		subscription.NextRetryAt = nil
		subscription.GraceEndsAt = nil

		if err := subService.Update(ctx, subscription); err != nil {
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		renewalApplied = true
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":           subscription.ID,
			"user_id":                   subscription.UserID,
			"price_id":                  price.ID,
			"processor":                 subscription.Processor,
			"processor_subscription_id": subscription.ProcessorSubscriptionID,
			"period_start":              periodStartsAt,
			"period_end":                periodEndsAt,
			"downgrade_applied":         applyingDowngrade,
		}).Info("Updated subscription for renewal")

		// Append the next paid entitlement window for the subscription's entitlements.
		// Entitlement windows are immutable: renewals create new windows instead of extending existing ones.
		effectiveProduct := newProduct
		if effectiveProduct == nil {
			effectiveProduct, err = productService.GetByID(ctx, price.ProductID)
			if err != nil {
				return fmt.Errorf("failed to get product for renewal: %w", err)
			}
		}
		if effectiveProduct != nil && effectiveProduct.EntitlementsSpec != nil {
			notBefore := periodStartsAt.UTC()
			endAt := periodEndsAt.UTC()
			for entName := range effectiveProduct.EntitlementsSpec {
				// If the subscription had processor-driven grace windows (e.g. CCBill dunning),
				// remove them before pushing the next paid window. Otherwise, the grace tail can
				// cause the paid push to be scheduled after grace or become a no-op.
				grace := models.EntitlementSourceGrace
				sid := subscription.ID
				if err := entitlementService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
					UserID:      subscription.UserID,
					Entitlement: entName,
					SourceType:  &grace,
					SourceID:    &sid,
					Reason:      models.EntitlementRevokeSuperseded,
				}); err != nil {
					return fmt.Errorf("failed to clear grace entitlement %s on renewal: %w", entName, err)
				}

				if _, err := entitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
					UserID:      subscription.UserID,
					Entitlement: entName,
					NotBefore:   &notBefore,
					EndAt:       &endAt,
					SourceType:  models.EntitlementSourceSubscription,
					SourceID:    subscription.ID,
				}); err != nil {
					return fmt.Errorf("failed to grant renewal entitlement %s: %w", entName, err)
				}
			}
		}

		// Handle entitlements for downgrade
		if applyingDowngrade && oldProduct != nil && newProduct != nil {
			// Determine which entitlements to revoke (in old but not in new)
			oldEnts := make(map[string]bool)
			for name := range oldProduct.EntitlementsSpec {
				oldEnts[name] = true
			}

			newEnts := make(map[string]bool)
			for name := range newProduct.EntitlementsSpec {
				newEnts[name] = true
			}

			// Revoke entitlements that are in old product but not in new product
			for entName := range oldEnts {
				if !newEnts[entName] {
					reason := models.EntitlementRevokeDowngrade
					st := models.EntitlementSourceSubscription
					sid := subscription.ID
					if err := entitlementService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
						UserID:      subscription.UserID,
						Entitlement: entName,
						SourceType:  &st,
						SourceID:    &sid,
						Reason:      reason,
					}); err != nil {
						log.WithContext(ctx).WithError(err).WithFields(log.Fields{
							"subscription_id": subscription.ID,
							"entitlement":     entName,
						}).Warn("Failed to revoke entitlement during downgrade")
					} else {
						log.WithContext(ctx).WithFields(log.Fields{
							"subscription_id": subscription.ID,
							"entitlement":     entName,
						}).Info("Revoked entitlement due to downgrade")
					}
				}
			}

			// Any new entitlements introduced by the downgrade target product are granted by the renewal push above.
		}

		// Notify user
		eventType := models.NotificationPremiumRenewed
		var notifData map[string]any
		if applyingDowngrade && newProduct != nil {
			notifData = map[string]any{
				"downgrade_applied": true,
				"new_product":       newProduct.DisplayName,
			}
		}

		notification := &models.NotificationQueue{
			ID:        uuid.New(),
			UserID:    subscription.UserID,
			EventType: eventType,
			Data:      notifData,
		}
		if err := notificationRepo.Create(ctx, notification); err != nil {
			log.WithContext(ctx).WithError(err).Error("failed to create membership renewed notification")
		} else {
			notifications = append(notifications, notification)
		}

		return nil
	})

	if err != nil {
		return err
	}
	if !renewalApplied {
		return nil
	}

	s.dispatchNotifications(ctx, notifications)

	// Log the charge success event to ClickHouse
	if s.EventLogService != nil {
		sub := &models.Subscription{ID: subscriptionID, UserID: userID}
		if err := s.EventLogService.LogLifecycleChargeSuccess(ctx, sub, params.Processor, params.TransactionID, params.Amount, params.Currency, s.now(), map[string]interface{}{"renewal": true}); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscriptionID,
				"event_type":      "charge_success",
			}).Warn("failed to log renewal payment event to ClickHouse")
		}
	}

	return nil
}

// ReactivateMembership reactivates a previously cancelled subscription and restores
// its paid entitlement windows for the current product tier.
func (s *SubscriptionLifecycleService) ReactivateMembership(ctx context.Context, params *ReactivateMembershipParams) (*models.Subscription, error) {
	if params == nil {
		return nil, fmt.Errorf("reactivation params are required")
	}

	processorSubID := strings.TrimSpace(params.ProcessorSubscriptionID)
	if processorSubID == "" {
		return nil, fmt.Errorf("processor subscription id is required")
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"processor":                 params.Processor,
		"processor_subscription_id": processorSubID,
		"has_period_override":       params.CurrentPeriodEndsAt != nil,
		"allow_terminal_reactivate": params.AllowTerminalReactivation,
	}).Info("Starting membership reactivation flow")

	var reactivated *models.Subscription

	err := s.DB.GetDB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		txdb := db.NewWithTx(tx)
		priceService := catalog.NewPriceService(txdb)
		productService := catalog.NewProductService(txdb)
		subService := NewSubscriptionService(txdb, priceService, productService, nil, nil, nil)
		entitlementService := entitlements.NewEntitlementService(txdb)
		entitlementService.SetClock(s.Clock)

		subscription, err := subService.GetByProcessorSubscriptionID(ctx, string(params.Processor), processorSubID)
		if err != nil {
			return fmt.Errorf("failed to get subscription for reactivation: %w", err)
		}

		if err := s.assertActiveTransitionAllowed(ctx, subscription, "reactivation", params.AllowTerminalReactivation); err != nil {
			return err
		}

		price, err := priceService.GetByID(ctx, subscription.PriceID)
		if err != nil {
			return fmt.Errorf("failed to load subscription price: %w", err)
		}

		product, err := productService.GetByID(ctx, price.ProductID)
		if err != nil {
			return fmt.Errorf("failed to load subscription product: %w", err)
		}

		now := s.now().UTC()
		periodStartsAt := now
		var periodEndsAt time.Time
		if params.CurrentPeriodEndsAt != nil && !params.CurrentPeriodEndsAt.IsZero() && params.CurrentPeriodEndsAt.After(periodStartsAt) {
			periodEndsAt = params.CurrentPeriodEndsAt.UTC()
		} else if price.BillingCycleDays != nil && *price.BillingCycleDays > 0 {
			periodEndsAt = now.Add(time.Duration(*price.BillingCycleDays) * 24 * time.Hour)
		} else {
			periodEndsAt = now.Add(30 * 24 * time.Hour)
		}

		subscription.Status = models.StatusActive
		subscription.CurrentPeriodStartsAt = &periodStartsAt
		subscription.CurrentPeriodEndsAt = &periodEndsAt
		subscription.CancelledAt = nil
		subscription.CancelType = nil
		subscription.CancelFeedback = nil
		subscription.EndedAt = nil
		subscription.LastRetryAt = nil
		subscription.RetryAttempts = nil
		subscription.NextRetryAt = nil
		subscription.GraceEndsAt = nil

		if err := subService.Update(ctx, subscription); err != nil {
			return fmt.Errorf("failed to update reactivated subscription: %w", err)
		}

		entNames := make([]string, 0)
		if product.EntitlementsSpec != nil && len(product.EntitlementsSpec) > 0 {
			entNames = make([]string, 0, len(product.EntitlementsSpec))
			for name := range product.EntitlementsSpec {
				entNames = append(entNames, name)
			}
		} else {
			entNames = append(entNames, "premium")
		}

		notBefore := periodStartsAt.UTC()
		endAt := periodEndsAt.UTC()
		graceSource := models.EntitlementSourceGrace
		subSource := models.EntitlementSourceSubscription
		subID := subscription.ID

		for _, entName := range entNames {
			if err := entitlementService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
				UserID:      subscription.UserID,
				Entitlement: entName,
				SourceType:  &graceSource,
				SourceID:    &subID,
				Reason:      models.EntitlementRevokeSuperseded,
			}); err != nil {
				return fmt.Errorf("failed to clear grace entitlement %s on reactivation: %w", entName, err)
			}

			if _, err := entitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
				UserID:      subscription.UserID,
				Entitlement: entName,
				NotBefore:   &notBefore,
				EndAt:       &endAt,
				SourceType:  subSource,
				SourceID:    subID,
			}); err != nil {
				return fmt.Errorf("failed to restore entitlement %s on reactivation: %w", entName, err)
			}
		}

		reactivated = subscription
		return nil
	})
	if err != nil {
		return nil, err
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":           reactivated.ID,
		"user_id":                   reactivated.UserID,
		"processor":                 reactivated.Processor,
		"processor_subscription_id": reactivated.ProcessorSubscriptionID,
		"current_period_ends_at":    reactivated.CurrentPeriodEndsAt,
	}).Info("Membership reactivation flow completed")

	return reactivated, nil
}

// CancelMembership cancels a subscription and revokes associated roles
func (s *SubscriptionLifecycleService) CancelMembership(ctx context.Context, params *CancelMembershipParams) error {
	notifications := make([]*models.NotificationQueue, 0, 1)

	// Variables to capture from transaction for event logging
	var subscriptionID uuid.UUID
	var userID string
	var processor models.Processor

	var procName string
	if params.Processor != nil {
		procName = string(*params.Processor)
	}
	procSub := normalize.FromPtr(params.ProcessorSubscriptionID)
	subID := ""
	if params.SubscriptionID != nil {
		subID = params.SubscriptionID.String()
	}
	cancelFeedback := normalize.FromPtr(params.CancelFeedback)
	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":           subID,
		"processor":                 procName,
		"processor_subscription_id": procSub,
		"cancel_type":               params.CancelType,
		"revoke_access_immediately": params.RevokeAccess,
		"cancel_feedback_provided":  cancelFeedback != "",
	}).Info("Starting membership cancellation flow")

	err := s.DB.GetDB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		db := db.NewWithTx(tx)
		priceService := catalog.NewPriceService(db)
		productService := catalog.NewProductService(db)
		notificationRepo := repo.NewNotificationQueueRepo(db)
		subService := NewSubscriptionService(db, priceService, productService, nil, nil, nil)
		entSvc := entitlements.NewEntitlementService(db)
		entSvc.SetClock(s.Clock) // Propagate clock for testing

		// Use processor name for gateway lookup
		// Find subscription
		var subscription *models.Subscription
		var err error

		if params.SubscriptionID != nil {
			subscription, err = subService.GetByID(ctx, *params.SubscriptionID)
		} else if params.ProcessorSubscriptionID != nil && params.Processor != nil {
			subscription, err = subService.GetByProcessorSubscriptionID(ctx, string(*params.Processor), *params.ProcessorSubscriptionID)
		} else {
			return fmt.Errorf("either subscription_id or processor details must be provided")
		}

		if err != nil {
			log.WithContext(ctx).WithError(err).Warn("Failed to locate subscription for cancellation")
			return fmt.Errorf("subscription not found: %w", err)
		}

		// Capture values for event logging after transaction
		subscriptionID = subscription.ID
		userID = subscription.UserID
		processor = subscription.Processor

		// Update subscription status
		now := s.now()
		endAt := now
		if params.RevokeAccess {
			// Immediate revocation
			subscription.CurrentPeriodEndsAt = &now
			// Keep period bounds valid when revoking a future-dated window.
			// Some records may have CurrentPeriodStartsAt in the future due to precomputed renewals.
			if subscription.CurrentPeriodStartsAt != nil && !subscription.CurrentPeriodStartsAt.Before(now) {
				adjustedStart := now.Add(-time.Second)
				subscription.CurrentPeriodStartsAt = &adjustedStart
			}
		} else if subscription.CurrentPeriodEndsAt != nil && subscription.CurrentPeriodEndsAt.After(now) {
			// Period-end cancellation: keep access until paid term ends.
			endAt = *subscription.CurrentPeriodEndsAt
		}

		subscription.Status = models.StatusCancelled
		subscription.EndedAt = &endAt
		subscription.CancelType = &params.CancelType
		subscription.CancelFeedback = params.CancelFeedback
		subscription.CancelledAt = &now

		if err := subService.Update(ctx, subscription); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscription.ID,
			}).Error("Failed to update subscription during cancellation")
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"user_id":         subscription.UserID,
			"status":          subscription.Status,
			"ended_at":        subscription.EndedAt,
			"period_end":      subscription.CurrentPeriodEndsAt,
		}).Info("Updated subscription record during cancellation")

		// Entitlement windows are immutable; period-end cancellations require no entitlement mutation.
		// Only immediate cancellations/revocations remove access now by revoking the subscription's entitlement windows.
		if entSvc != nil && (params.RevokeAccess || subscription.CurrentPeriodEndsAt == nil || !subscription.CurrentPeriodEndsAt.After(now)) {
			revokeReason := models.EntitlementRevokeAdmin
			if params.CancelType == models.CancelTypeChargeback {
				revokeReason = models.EntitlementRevokeChargeback
			}

			names, err := entSvc.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceSubscription, subscription.ID)
			if err != nil {
				log.WithContext(ctx).WithError(err).Error("failed to list entitlements for cancelled subscription")
			} else {
				st := models.EntitlementSourceSubscription
				sid := subscription.ID
				for _, entName := range names {
					if err := entSvc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
						UserID:      subscription.UserID,
						Entitlement: entName,
						SourceType:  &st,
						SourceID:    &sid,
						Reason:      revokeReason,
					}); err != nil {
						log.WithContext(ctx).WithError(err).WithFields(log.Fields{
							"subscription_id": subscription.ID,
							"entitlement":     entName,
						}).Error("failed to revoke entitlement during cancellation")
					}
				}
			}
		}

		reason := PremiumEndReasonAdmin
		switch params.CancelType {
		case models.CancelTypeUser:
			reason = PremiumEndReasonUserCancel
		case models.CancelTypeExpired:
			reason = PremiumEndReasonExpired
		case models.CancelTypeMerchant:
			reason = PremiumEndReasonProcessor
		}

		notification := &models.NotificationQueue{
			ID:        uuid.New(),
			UserID:    subscription.UserID,
			EventType: models.NotificationPremiumEnded,
			Data:      map[string]any{"reason": string(reason)},
		}
		if err := notificationRepo.Create(ctx, notification); err != nil {
			log.WithContext(ctx).WithError(err).Error("failed to create membership ended notification")
		} else {
			notifications = append(notifications, notification)
		}

		return nil
	})

	if err != nil {
		return err
	}

	s.dispatchNotifications(ctx, notifications)
	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id": subscriptionID,
		"user_id":         userID,
	}).Info("Membership cancellation flow completed")

	// Log the subscription cancelled event to ClickHouse
	if s.EventLogService != nil {
		if err := s.EventLogService.LogLifecycleCancellation(ctx, subscriptionID, userID, processor, params.CancelType, params.RevokeAccess, s.now()); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscriptionID,
				"event_type":      "subscription_cancelled",
			}).Warn("failed to log subscription cancelled event to ClickHouse")
		}
	}

	return nil
}

// ExpireMembership expires a subscription and revokes associated roles
func (s *SubscriptionLifecycleService) ExpireMembership(ctx context.Context, subscriptionID uuid.UUID) error {
	notifications := make([]*models.NotificationQueue, 0, 1)

	log.WithContext(ctx).WithField("subscription_id", subscriptionID).Info("Starting membership expiration flow")

	err := s.DB.GetDB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		db := db.NewWithTx(tx)
		priceService := catalog.NewPriceService(db)
		productService := catalog.NewProductService(db)
		notificationRepo := repo.NewNotificationQueueRepo(db)
		subService := NewSubscriptionService(db, priceService, productService, nil, nil, nil)
		entSvc := entitlements.NewEntitlementService(db)
		entSvc.SetClock(s.Clock) // Propagate clock for testing

		subscription, err := subService.GetByID(ctx, subscriptionID)
		if err != nil {
			log.WithContext(ctx).WithError(err).Warn("Failed to locate subscription for expiration")
			return fmt.Errorf("subscription not found: %w", err)
		}

		// Update subscription status - Wave 18: expired = cancelled (never rebill again)
		now := s.now()
		subscription.Status = models.StatusCancelled
		subscription.CancelledAt = &now
		expired := models.CancelTypeExpired
		subscription.CancelType = &expired
		subscription.EndedAt = &now

		if err := subService.Update(ctx, subscription); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscription.ID,
			}).Error("Failed to update subscription during expiration")
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"user_id":         subscription.UserID,
		}).Info("Marked subscription as expired")

		// Revoke entitlements
		if entSvc != nil {
			names, err := entSvc.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceSubscription, subscription.ID)
			if err != nil {
				log.WithContext(ctx).WithError(err).Error("failed to list entitlements for expired subscription")
			} else {
				st := models.EntitlementSourceSubscription
				sid := subscription.ID
				for _, entName := range names {
					if err := entSvc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
						UserID:      subscription.UserID,
						Entitlement: entName,
						SourceType:  &st,
						SourceID:    &sid,
						Reason:      models.EntitlementRevokeDunning,
					}); err != nil {
						log.WithContext(ctx).WithError(err).WithFields(log.Fields{
							"subscription_id": subscription.ID,
							"entitlement":     entName,
						}).Error("failed to revoke entitlement for expired subscription")
					}
				}
			}

			// Terminal expiration: immediately remove any grace windows for this subscription too.
			graceNames, err := entSvc.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceGrace, subscription.ID)
			if err != nil {
				log.WithContext(ctx).WithError(err).Error("failed to list grace entitlements for expired subscription")
			} else {
				st := models.EntitlementSourceGrace
				sid := subscription.ID
				for _, entName := range graceNames {
					if err := entSvc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
						UserID:      subscription.UserID,
						Entitlement: entName,
						SourceType:  &st,
						SourceID:    &sid,
						Reason:      models.EntitlementRevokeDunning,
					}); err != nil {
						log.WithContext(ctx).WithError(err).WithFields(log.Fields{
							"subscription_id": subscription.ID,
							"entitlement":     entName,
						}).Error("failed to revoke grace entitlement for expired subscription")
					}
				}
			}
		}

		notification := &models.NotificationQueue{
			ID:        uuid.New(),
			UserID:    subscription.UserID,
			EventType: models.NotificationPremiumEnded,
			Data:      map[string]any{"reason": string(PremiumEndReasonExpired)},
		}
		if err := notificationRepo.Create(ctx, notification); err != nil {
			log.WithContext(ctx).WithError(err).Error("failed to create membership expired notification")
		} else {
			notifications = append(notifications, notification)
		}

		return nil
	})

	if err != nil {
		return err
	}

	s.dispatchNotifications(ctx, notifications)
	log.WithContext(ctx).WithField("subscription_id", subscriptionID).Info("Membership expiration flow completed")

	return nil
}

// FailMembership marks a subscription as failed due to payment issues.
//
// Behavior depends on config.FeatureFlags.DunningMode:
//   - "on" or "dry_run_only": Normal dunning flow - go to past_due, schedule retries
//   - "off": Immediate cancellation - no grace period, no retries
func (s *SubscriptionLifecycleService) FailMembership(ctx context.Context, params *FailMembershipParams) error {
	notifications := make([]*models.NotificationQueue, 0, 1)

	// Variables to capture from transaction for event logging
	var subscriptionID uuid.UUID
	var userID string
	var finalStatus models.SubscriptionStatus

	// Check dunning mode from feature flags
	dunningMode := config.DunningModeOn
	if s.Config != nil {
		dunningMode = s.Config.GetDunningMode()
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"processor":                 params.Processor,
		"processor_subscription_id": params.SubscriptionID,
		"failure_reason":            normalize.FromPtr(params.FailureReason),
		"failure_code":              normalize.FromPtr(params.FailureCode),
		"dunning_mode":              dunningMode,
	}).Warn("Starting membership failure flow")

	err := s.DB.GetDB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		db := db.NewWithTx(tx)
		priceService := catalog.NewPriceService(db)
		productService := catalog.NewProductService(db)
		notificationRepo := repo.NewNotificationQueueRepo(db)
		subService := NewSubscriptionService(db, priceService, productService, nil, nil, nil)
		entSvc := entitlements.NewEntitlementService(db)
		entSvc.SetClock(s.Clock) // Propagate clock for testing

		subscription, err := subService.GetByID(ctx, *params.SubscriptionID)
		if err != nil {
			log.WithContext(ctx).WithError(err).Warn("Failed to locate subscription for failure flow")
			return fmt.Errorf("subscription not found: %w", err)
		}

		// Capture values for event logging
		subscriptionID = subscription.ID
		userID = subscription.UserID

		now := s.now()

		// If dunning is off, immediately cancel - no grace period, no retries
		if dunningMode == config.DunningModeOff {
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.UserID,
			}).Warn("Dunning mode is 'off'; immediately cancelling subscription (no recovery)")

			subscription.Status = models.StatusCancelled
			subscription.EndedAt = &now
			// Don't set retry fields - we're not doing dunning
		} else {
			// Normal dunning flow (for "on" or "dry_run_only" modes)
			// Update subscription status - failed payment = past_due (still trying to recover)
			subscription.Status = models.StatusPastDue

			// Dunning policy for NMI-backed subscriptions: try every 3 days, up to 5 failures total
			// Example timeline (D = day of initial failure): D+3, D+6, D+9, D+12, D+15
			subscription.LastRetryAt = &now
			if subscription.RetryAttempts == nil {
				attempts := 1
				subscription.RetryAttempts = &attempts
			} else {
				*subscription.RetryAttempts++
			}

			// If we've reached MaxDunningFailures, cancel; otherwise schedule next attempt in DunningInterval
			if *subscription.RetryAttempts >= MaxDunningFailures {
				//subscription.Status = models.StatusCancelled
				//subscription.CancelledAt = &now
				// Ensure EndedAt is equal to or after CancelledAt to satisfy DB constraint
				//subscription.EndedAt = &now

				expired := models.CancelTypeExpired
				subscription.Cancel("transaction_failure", &expired)
			} else {
				nextRetry := now.Add(DunningInterval)
				subscription.NextRetryAt = &nextRetry
			}
		}

		// For NMI-backed processors, we control retry timing; if the retry schedule would extend beyond
		// the paid term end, model that access as explicit grace entitlement windows.
		if processors.IsNMIBackedProcessor(subscription.Processor) && subscription.Status == models.StatusPastDue {
			if subscription.CurrentPeriodEndsAt != nil && subscription.NextRetryAt != nil && subscription.NextRetryAt.After(*subscription.CurrentPeriodEndsAt) {
				paidEnd := subscription.CurrentPeriodEndsAt.UTC()
				graceUntil := subscription.NextRetryAt.UTC()

				names, err := entSvc.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceSubscription, subscription.ID)
				if err != nil {
					log.WithContext(ctx).WithError(err).Error("failed to list subscription entitlements for grace append")
				} else {
					for _, entName := range names {
						notBefore := now.UTC()
						if paidEnd.After(notBefore) {
							notBefore = paidEnd
						}
						_, err := entSvc.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
							UserID:      subscription.UserID,
							Entitlement: entName,
							NotBefore:   &notBefore,
							EndAt:       &graceUntil,
							SourceType:  models.EntitlementSourceGrace,
							SourceID:    subscription.ID,
						})
						if err != nil {
							log.WithContext(ctx).WithError(err).WithFields(log.Fields{
								"subscription_id": subscription.ID,
								"entitlement":     entName,
							}).Error("failed to append grace entitlement window during dunning failure")
						}
					}
				}
			}
		}

		if err := subService.Update(ctx, subscription); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscription.ID,
			}).Error("Failed to update subscription during failure flow")
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"user_id":         subscription.UserID,
			"status":          subscription.Status,
			"retry_attempts":  subscription.RetryAttempts,
			"next_retry_at":   subscription.NextRetryAt,
		}).Warn("Updated subscription during failure flow")

		// Capture final status for event logging
		finalStatus = subscription.Status

		// Revoke entitlements if subscription is cancelled (after max retries)
		// Skip revocation if disable_entitlement_expiration flag is set
		if subscription.Status == models.StatusCancelled {
			if s.Config != nil && s.Config.IsEntitlementExpirationDisabled() {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
				}).Warn("Entitlement expiration disabled; skipping entitlement revocation (subscription still cancelled)")
			} else if entSvc != nil {
				names, err := entSvc.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceSubscription, subscription.ID)
				if err != nil {
					log.WithContext(ctx).WithError(err).Error("failed to list entitlements for failed subscription")
				} else {
					st := models.EntitlementSourceSubscription
					sid := subscription.ID
					for _, entName := range names {
						if err := entSvc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
							UserID:      subscription.UserID,
							Entitlement: entName,
							SourceType:  &st,
							SourceID:    &sid,
							Reason:      models.EntitlementRevokeDunning,
						}); err != nil {
							log.WithContext(ctx).WithError(err).WithFields(log.Fields{
								"subscription_id": subscription.ID,
								"entitlement":     entName,
							}).Error("failed to revoke entitlement for failed subscription")
						}
					}
					log.WithContext(ctx).WithFields(log.Fields{
						"subscription_id": subscription.ID,
					}).Warn("Revoked entitlements after max dunning failures")
				}

				// Terminal dunning failure: remove any grace windows too so access doesn't continue.
				graceNames, err := entSvc.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceGrace, subscription.ID)
				if err != nil {
					log.WithContext(ctx).WithError(err).Error("failed to list grace entitlements for failed subscription")
				} else {
					st := models.EntitlementSourceGrace
					sid := subscription.ID
					for _, entName := range graceNames {
						if err := entSvc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
							UserID:      subscription.UserID,
							Entitlement: entName,
							SourceType:  &st,
							SourceID:    &sid,
							Reason:      models.EntitlementRevokeDunning,
						}); err != nil {
							log.WithContext(ctx).WithError(err).WithFields(log.Fields{
								"subscription_id": subscription.ID,
								"entitlement":     entName,
							}).Error("failed to revoke grace entitlement for failed subscription")
						}
					}
				}
			}
		}

		eventType := models.NotificationPaymentMethodFailed
		if subscription.Status == models.StatusCancelled {
			eventType = models.NotificationPremiumEnded
		}

		var data map[string]any
		if eventType == models.NotificationPremiumEnded {
			data = map[string]any{"reason": string(PremiumEndReasonExpired)}
		}

		notification := &models.NotificationQueue{
			ID:        uuid.New(),
			UserID:    subscription.UserID,
			EventType: eventType,
			Data:      data,
		}
		if err := notificationRepo.Create(ctx, notification); err != nil {
			log.WithContext(ctx).WithError(err).Error("failed to create payment failed notification")
		} else {
			notifications = append(notifications, notification)
		}

		return nil
	})

	if err != nil {
		return err
	}

	s.dispatchNotifications(ctx, notifications)

	// Log the payment failure event to ClickHouse
	if s.EventLogService != nil {
		if err := s.EventLogService.LogLifecycleFailure(ctx, subscriptionID, userID, params.Processor, finalStatus, params.FailureReason, params.FailureCode, s.now()); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscriptionID,
				"event_type":      "charge_failure",
			}).Warn("failed to log payment failure event to ClickHouse")
		}
	}

	return nil
}

func (s *SubscriptionLifecycleService) logPaymentEvent(ctx context.Context, sub *models.Subscription, processor models.Processor, transactionID string, amount int64, currency string) {
	if s.EventLogService == nil || sub == nil {
		return
	}
	if err := s.EventLogService.LogLifecycleChargeSuccess(ctx, sub, processor, transactionID, amount, currency, s.now(), nil); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"subscription_id": sub.ID,
			"event_type":      "charge_success",
			"processor":       processor,
		}).Warn("failed to log payment event to ClickHouse")
	}
}

// Parameter structs for lifecycle operations
