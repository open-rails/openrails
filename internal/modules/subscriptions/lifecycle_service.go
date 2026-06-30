package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	repo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	log "github.com/sirupsen/logrus"
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

	// deferDelete enqueues the deferred NMI delete_subscription job (#344
	// follow-up). Optional: injected via SetDeferredDeleteScheduler in the
	// composition root (same pattern as UserSubscriptionService.deferDelete).
	// When nil, terminal dunning cancellations leave the remote NMI
	// subscription alive (caller-side paths or #107 reconciliation handle it).
	deferDelete DeferredDeleteScheduler
}

func (s *SubscriptionLifecycleService) assertActiveTransitionAllowed(ctx context.Context, subscription *models.Subscription, trigger string, allowOverride bool) error {
	reason, terminal := TerminalCancelReason(subscription)
	if !terminal {
		return nil
	}

	if allowOverride {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"rail":            subscription.Rail,
			"trigger":         trigger,
			"reason":          reason,
		}).Warn("Bypassing terminal transition guard via explicit manual override")
		return nil
	}

	return &TerminalTransitionBlockedError{
		SubscriptionID: subscription.ID,
		Rail:           subscription.Rail,
		FromStatus:     subscription.Status,
		ToStatus:       models.StatusActive,
		CancelType:     NormalizeCancelType(subscription.CancelType),
		Trigger:        trigger,
		Reason:         reason,
	}
}

// NewSubscriptionLifecycleService creates a new instance of SubscriptionLifecycleService
func NewSubscriptionLifecycleService(db *db.DB, productService *catalog.ProductService, priceService *catalog.PriceService, entitlementService *entitlements.EntitlementService, notificationService NotificationEmailSender, paymentService *payments.PaymentService, eventLogService LifecycleEventLogger, clocks ...clockwork.Clock) *SubscriptionLifecycleService {
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

// SetDeferredDeleteScheduler injects the deferred NMI delete scheduler (#344
// follow-up). Wired post-construction in the composition root once the River
// producer exists, mirroring UserSubscriptionService.SetDeferredDeleteScheduler.
func (s *SubscriptionLifecycleService) SetDeferredDeleteScheduler(d DeferredDeleteScheduler) {
	s.deferDelete = d
}

// now returns the current time from the service's clock
func (s *SubscriptionLifecycleService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// appendRenewalGraceWindows pre-appends the #368 trailing grace window
// [periodEnd, periodEnd + GraceSlack(cycle)) for each of the subscription's
// entitlements (source_type='grace', source_id=subscription id), so SILENCE
// at period end — a lost success webhook, a provider billing on its own day
// boundary, a merely late webhook — extends access briefly instead of cutting
// it at the period-end second. Called on activation and on every renewal for
// grace-eligible rails (NMI-backed + Stripe; see
// RenewalGraceEligibleRail), right after the paid window push, so the
// timeline tail is the paid period end and the grace window starts exactly
// there (no-gap property; the DB period range is half-open, so touching
// boundaries never violate the overlap exclusion).
//
// Bounded + revocable by design: truth arriving revokes it (renewal success
// revokes-then-pushes in RenewMembership; terminal failure revokes in
// FailMembership/ExpireMembership; a deliberate cancel deletes the scheduled
// grace in CancelMembership; the #367 liveness sync resolves the silence the
// grace is bridging). Still-silence past the slack just lapses by end_at —
// fail-closed eventually, no revocation sweep involved, so the
// DISABLE_ENTITLEMENT_EXPIRATION flag needs no special handling here.
//
// Idempotent: re-pushing the same (sub, period) grace lands in
// PushNewEntitlement's covered-window branch and returns the existing row.
// Months-stale period ends (e.g. imported subscriptions) get no resurrection
// grace: the push is skipped once periodEnd + slack is already in the past.
// Failures are logged, never returned — generosity must not fail the renewal.
func (s *SubscriptionLifecycleService) appendRenewalGraceWindows(
	ctx context.Context,
	entitlementService *entitlements.EntitlementService,
	subscription *models.Subscription,
	entNames []string,
	periodEnd time.Time,
	cycleHours int,
) {
	if entitlementService == nil || subscription == nil || periodEnd.IsZero() || len(entNames) == 0 {
		return
	}
	if !RenewalGraceEligibleRail(subscription.Rail) {
		return
	}
	notBefore := periodEnd.UTC()
	graceEnd := notBefore.Add(GraceSlack(cycleHours))
	if !graceEnd.After(s.now().UTC()) {
		return
	}
	for _, entName := range entNames {
		if _, err := entitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
			UserID:      subscription.CustomerID.String(),
			Entitlement: entName,
			NotBefore:   &notBefore,
			EndAt:       &graceEnd,
			SourceType:  models.EntitlementSourceGrace,
			SourceID:    subscription.ID,
		}); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"entitlement":     entName,
				"grace_end":       graceEnd,
			}).Error("failed to pre-append renewal grace window")
		}
	}
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
				"user_id":         notification.CustomerID.String(),
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

	procSubID := normalize.FromPtr(params.RailSubscriptionID)

	log.WithContext(ctx).WithFields(log.Fields{
		"user_id":              params.UserID,
		"price_id":             params.PriceID,
		"rail":                 params.Rail,
		"rail_subscription_id": procSubID,
		"transaction_id":       params.TransactionID,
		"amount_cents":         params.Amount,
		"currency":             params.Currency,
	}).Info("Starting membership creation flow")

	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		dbb := db.NewWithPgxTx(tx)
		var err error
		subscription, notifications, err = s.createMembershipCore(ctx, dbb, params)
		return err
	})
	if err != nil {
		return nil, err
	}

	s.dispatchNotifications(ctx, notifications)

	// Log the charge success event to ClickHouse
	s.logPaymentEvent(ctx, subscription, params.Rail, params.TransactionID, params.Amount, params.Currency)

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
	subService := NewSubscriptionService(dbb, priceService, productService, nil, nil, nil, s.Clock())

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
	if params.RailSubscriptionID != nil && strings.TrimSpace(*params.RailSubscriptionID) != "" {
		found, err := subService.GetByRailSubscriptionID(ctx, string(params.Rail), strings.TrimSpace(*params.RailSubscriptionID))
		if err != nil && !repo.IsNotFound(err) {
			return nil, nil, fmt.Errorf("failed to check existing subscription by rail subscription ID: %w", err)
		}
		if err == nil && found.Status == models.StatusPending {
			if found.CustomerID.String() != params.UserID || found.ProductID != price.ProductID {
				return nil, nil, fmt.Errorf("rail subscription belongs to a different pending subscription")
			}
			existingPendingSub = found
		}
	}

	var paymentService *payments.PaymentService
	if params.TransactionID != "" && s.PaymentService != nil {
		paymentService = payments.NewPaymentService(dbb, s.Clock())
		existingPayment, err := paymentService.GetByTransactionID(ctx, params.Rail, params.TransactionID)
		if err != nil && !repo.IsNotFound(err) {
			return nil, nil, fmt.Errorf("failed to check existing payment: %w", err)
		}
		if err == nil {
			if existingPayment.CustomerID.String() != params.UserID {
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
			if existingSubscription.CustomerID.String() != params.UserID {
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
	if err != nil && !repo.IsNotFound(err) {
		return nil, nil, fmt.Errorf("failed to check existing subscriptions: %w", err)
	}

	if err == nil && (existingPendingSub == nil || activeSub.ID != existingPendingSub.ID) {
		log.WithContext(ctx).WithFields(log.Fields{
			"user_id":                  params.UserID,
			"product_id":               price.ProductID,
			"existing_subscription_id": activeSub.ID,
			"existing_price_id":        activeSub.PriceID,
			"existing_status":          activeSub.Status,
			"existing_rail":            activeSub.Rail,
			"rail_subscription_id":     activeSub.RailSubscriptionID,
		}).Warn("User already has an active, pending, or past_due subscription for this product; aborting membership creation")
		return nil, nil, fmt.Errorf("user already has an active, pending, or past_due subscription for this product")
	}

	now := s.now()
	periodStartsAt := now
	if params.CurrentPeriodStartsAt != nil && !params.CurrentPeriodStartsAt.IsZero() {
		periodStartsAt = params.CurrentPeriodStartsAt.UTC()
	}
	var periodEndsAt time.Time
	switch {
	case params.CurrentPeriodEndsAt != nil && !params.CurrentPeriodEndsAt.IsZero() && params.CurrentPeriodEndsAt.After(periodStartsAt):
		periodEndsAt = params.CurrentPeriodEndsAt.UTC()
	case price.AccessDurationHours != nil:
		// Truthful window from the price's declared access duration — covers both
		// recurring and one-off/durable prices (RecurringCycleHours is AutoRenew-gated).
		periodEndsAt = now.Add(time.Duration(*price.AccessDurationHours) * time.Hour)
	default:
		// #651: no provider period and no declared access duration. Don't silently
		// invent a cadence — warn, then fall back to 30d so the row stays valid.
		log.WithContext(ctx).WithFields(log.Fields{
			"price_id":   price.ID,
			"product_id": price.ProductID,
			"user_id":    params.UserID,
		}).Warn("price has no access duration and provider supplied no period end; defaulting membership period to 30d")
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
		existingPendingSub.Rail = params.Rail
		if params.RailSubscriptionID != nil {
			existingPendingSub.RailSubscriptionID = *params.RailSubscriptionID
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
		existingPendingSub.CancelFeedback = nil
		existingPendingSub.EndedAt = nil

		if err := subService.Update(ctx, existingPendingSub); err != nil {
			return nil, nil, fmt.Errorf("failed to update subscription: %w", err)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":      existingPendingSub.ID,
			"user_id":              existingPendingSub.CustomerID.String(),
			"price_id":             existingPendingSub.PriceID,
			"rail":                 existingPendingSub.Rail,
			"rail_subscription_id": existingPendingSub.RailSubscriptionID,
			"period_start":         periodStartsAt,
			"period_end":           periodEndsAt,
		}).Info("Activating existing pending subscription record for membership creation")
		subscription = existingPendingSub
	} else {
		subscription = &models.Subscription{
			ID:                       uuidutil.NewV7(),
			CustomerID:               identity.CustomerIDFromString(params.UserID).UUID(),
			ProductID:                price.ProductID,
			PriceID:                  price.ID,
			EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(product.EntitlementsSpec),
			CreditsSpecSnapshot:      models.CloneCreditsSpec(product.CreditsSpec),
			Status:                   models.StatusActive,
			Rail:                     params.Rail,
			RailSubscriptionID: func() string {
				if params.RailSubscriptionID != nil {
					return *params.RailSubscriptionID
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
			"subscription_id":      subscription.ID,
			"user_id":              subscription.CustomerID.String(),
			"price_id":             subscription.PriceID,
			"rail":                 subscription.Rail,
			"rail_subscription_id": subscription.RailSubscriptionID,
			"period_start":         periodStartsAt,
			"period_end":           periodEndsAt,
		}).Info("Created new subscription record for membership")
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
			// #651: never fabricate a "premium" entitlement nobody declared. Grant
			// what the product specifies (here: nothing) and warn so an empty spec
			// surfaces as misconfiguration instead of silent access.
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"product_id":      price.ProductID,
				"user_id":         subscription.CustomerID.String(),
			}).Warn("subscription product declares no entitlements; granting none (was fabricating \"premium\")")
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"user_id":         subscription.CustomerID.String(),
			"entitlements":    entNames,
		}).Info("Preparing to grant subscription entitlements")

		// A membership created with an ALREADY-elapsed period (stale import /
		// backfill shapes) grants no access window — the period is over.
		if !periodEndsAt.UTC().After(s.now().UTC()) {
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"period_end":      periodEndsAt,
			}).Warn("Membership period already elapsed; creating lifecycle records without granting entitlement windows")
			entNames = nil
		}

		for _, ent := range entNames {
			existsBySource, err := entitlementService.ExistsBySource(ctx, models.EntitlementSourceSubscription, subscription.ID, ent)
			if err != nil {
				return nil, nil, fmt.Errorf("failed entitlement check: %w", err)
			}
			if existsBySource {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"user_id":         subscription.CustomerID.String(),
					"entitlement":     ent,
				}).Info("Entitlement already granted for subscription; skipping")
				continue
			}

			notBefore := periodStartsAt.UTC()
			endAt := periodEndsAt.UTC()
			window, err := entitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
				UserID:      subscription.CustomerID.String(),
				Entitlement: ent,
				NotBefore:   &notBefore,
				EndAt:       &endAt,
				SourceType:  models.EntitlementSourceSubscription,
				SourceID:    subscription.ID,
			})
			if err != nil {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"user_id":         subscription.CustomerID.String(),
					"entitlement":     ent,
				}).WithError(err).Error("Failed to grant subscription entitlement")
				return nil, nil, fmt.Errorf("failed to grant entitlement %s: %w", ent, err)
			}
			if window == nil {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"user_id":         subscription.CustomerID.String(),
					"entitlement":     ent,
				}).Info("Subscription entitlement already covered by canonical timeline")
				continue
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.CustomerID.String(),
				"entitlement":     ent,
				"window_start":    window.StartAt,
				"window_end":      window.EndAt,
			}).Info("Granted subscription entitlement")
		}

		// #368: pre-append the trailing renewal grace window right behind the
		// paid window, so silence at the first renewal never gates the user.
		s.appendRenewalGraceWindows(ctx, entitlementService, subscription, entNames, periodEndsAt, BillingCycleHoursOf(price))
	}

	notification := &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: subscription.CustomerID,
		EventType:  models.NotificationPremiumStarted,
	}
	if err := notificationRepo.Create(ctx, notification); err != nil {
		log.WithContext(ctx).WithError(err).Error("failed to create membership started notification")
	} else {
		notifications = append(notifications, notification)
	}

	// Create Payment record if payment info is provided
	if params.TransactionID != "" && paymentService != nil {
		// Use provided amount/currency or fall back to price defaults
		amount := params.Amount
		if !params.AmountProvided && amount == 0 {
			// #651: caller supplied no charged amount; record the catalog list price
			// but warn — this is the expected price, not a confirmed charged amount.
			log.WithContext(ctx).WithFields(log.Fields{
				"price_id":       price.ID,
				"transaction_id": params.TransactionID,
				"user_id":        subscription.CustomerID.String(),
			}).Warn("no charged amount supplied for membership payment; recording catalog list price")
			amount = price.Amount
		}
		currency := params.Currency
		if currency == "" {
			currency = price.Currency
		}

		// #651: record the provider's transaction time when supplied; now() only as
		// last resort. Status is set explicitly (was relying on the SQL empty->'completed').
		purchasedAt := now
		if params.PurchasedAt != nil && !params.PurchasedAt.IsZero() {
			purchasedAt = params.PurchasedAt.UTC()
		}
		payment := &models.Payment{
			ID:                       uuidutil.NewV7(),
			CustomerID:               subscription.CustomerID,
			PriceID:                  price.ID,
			SubscriptionID:           &subscription.ID,
			Rail:                     params.Rail,
			TransactionID:            params.TransactionID,
			Amount:                   amount,
			ListAmount:               price.Amount,
			Currency:                 currency,
			Status:                   payments.PaymentStatusCompletedValue,
			Metadata:                 params.PaymentMetadata,
			EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(subscription.EntitlementsSpecSnapshot),
			CreditsSpecSnapshot:      models.CloneCreditsSpec(subscription.CreditsSpecSnapshot),
			PurchasedAt:              purchasedAt,
			CreatedAt:                now,
		}
		if err := paymentService.Create(ctx, payment); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"transaction_id":  params.TransactionID,
				"subscription_id": subscription.ID,
				"user_id":         subscription.CustomerID.String(),
			}).Error("failed to create payment record for new membership")
			return nil, nil, fmt.Errorf("failed to create payment record for new membership: %w", err)
		} else {
			log.WithContext(ctx).WithFields(log.Fields{
				"transaction_id":  params.TransactionID,
				"subscription_id": subscription.ID,
				"user_id":         subscription.CustomerID.String(),
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
		"rail":                      params.Rail,
		"rail_subscription_id":      params.RailSubscriptionID,
		"transaction_id":            params.TransactionID,
		"amount_cents":              params.Amount,
		"currency":                  params.Currency,
		"allow_terminal_reactivate": params.AllowTerminalReactivation,
	}).Info("Starting membership renewal flow")

	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		db := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(db)
		productService := catalog.NewProductService(db)
		notificationRepo := repo.NewNotificationQueueRepo(db)
		subService := NewSubscriptionService(db, priceService, productService, nil, nil, nil, s.Clock())
		entitlementService := entitlements.NewEntitlementService(db, s.Clock())
		paymentService := payments.NewPaymentService(db, s.Clock())
		entitlementService.SetClock(s.Clock())

		// Find subscription - use rail name for gateway lookup
		subscription, err := subService.GetByRailSubscriptionID(ctx, string(params.Rail), params.RailSubscriptionID)
		if err != nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"rail":                 params.Rail,
				"rail_subscription_id": params.RailSubscriptionID,
			}).WithError(err).Error("Failed to load subscription for renewal")
			return fmt.Errorf("subscription not found: %w", err)
		}

		if err := s.assertActiveTransitionAllowed(ctx, subscription, "renewal", params.AllowTerminalReactivation); err != nil {
			return err
		}

		// Capture values for Payment creation after transaction
		subscriptionID = subscription.ID
		userID = subscription.CustomerID.String()

		// Check for scheduled downgrade
		var price *models.Price
		var oldProduct, newProduct *models.Product
		applyingDowngrade := subscription.ScheduledPriceID != nil
		oldEntitlementsSpec := models.CloneEntitlementsSpec(subscription.EntitlementsSpecSnapshot)

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
				"user_id":         subscription.CustomerID.String(),
				"old_price_id":    subscription.PriceID,
				"new_price_id":    price.ID,
				"old_product":     oldProduct.DisplayName,
				"new_product":     newProduct.DisplayName,
			}).Info("Applying scheduled downgrade on renewal")

			// Update subscription to new price and product
			subscription.PriceID = price.ID
			subscription.ProductID = price.ProductID
			subscription.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(newProduct.EntitlementsSpec)
			subscription.CreditsSpecSnapshot = models.CloneCreditsSpec(newProduct.CreditsSpec)
			subscription.ScheduledPriceID = nil // Clear the scheduled downgrade
		} else {
			// Normal renewal - use current price
			price, err = priceService.GetByID(ctx, subscription.PriceID)
			if err != nil {
				return fmt.Errorf("failed to get price: %w", err)
			}
			if len(subscription.EntitlementsSpecSnapshot) == 0 || len(subscription.CreditsSpecSnapshot) == 0 {
				product, err := productService.GetByID(ctx, price.ProductID)
				if err != nil {
					return fmt.Errorf("failed to get product for renewal snapshot: %w", err)
				}
				if len(subscription.EntitlementsSpecSnapshot) == 0 {
					subscription.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(product.EntitlementsSpec)
				}
				if len(subscription.CreditsSpecSnapshot) == 0 {
					subscription.CreditsSpecSnapshot = models.CloneCreditsSpec(product.CreditsSpec)
				}
			}
		}

		amount := params.Amount
		if !params.AmountProvided && amount <= 0 {
			amount = price.Amount
		}
		currency := strings.TrimSpace(params.Currency)
		if currency == "" {
			currency = price.Currency
		}

		if params.TransactionID != "" {
			now := s.now().UTC()
			if !params.AmountProvided && params.Amount <= 0 {
				// #651: recording a renewal payment with no charged amount supplied;
				// fall back to catalog list price but warn (expected price, not a
				// confirmed charge).
				log.WithContext(ctx).WithFields(log.Fields{
					"price_id":             price.ID,
					"rail_subscription_id": params.RailSubscriptionID,
					"transaction_id":       params.TransactionID,
				}).Warn("no charged amount supplied for renewal payment; recording catalog list price")
			}
			// #651: provider transaction time when supplied; now() only as last resort.
			purchasedAt := now
			if params.PurchasedAt != nil && !params.PurchasedAt.IsZero() {
				purchasedAt = params.PurchasedAt.UTC()
			}
			payment := &models.Payment{
				ID:                       uuidutil.NewV7(),
				CustomerID:               subscription.CustomerID,
				PriceID:                  price.ID,
				SubscriptionID:           &subscription.ID,
				Rail:                     params.Rail,
				TransactionID:            params.TransactionID,
				Amount:                   amount,
				ListAmount:               amount,
				Currency:                 currency,
				Status:                   payments.PaymentStatusCompletedValue,
				Metadata:                 params.PaymentMetadata,
				EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(subscription.EntitlementsSpecSnapshot),
				CreditsSpecSnapshot:      models.CloneCreditsSpec(subscription.CreditsSpecSnapshot),
				PurchasedAt:              purchasedAt,
				CreatedAt:                now,
			}
			created, err := paymentService.CreateIfNotExists(ctx, payment)
			if err != nil {
				return fmt.Errorf("failed to persist renewal payment marker: %w", err)
			}
			if !created {
				existingPayment, loadErr := paymentService.GetByTransactionID(ctx, params.Rail, params.TransactionID)
				if loadErr != nil {
					return fmt.Errorf("failed to load duplicate renewal payment marker: %w", loadErr)
				}
				if existingPayment.SubscriptionID == nil || *existingPayment.SubscriptionID != subscription.ID {
					return fmt.Errorf("duplicate renewal payment marker belongs to a different subscription")
				}
				if existingPayment.CustomerID.String() != subscription.CustomerID.String() {
					return fmt.Errorf("duplicate renewal payment marker belongs to a different user")
				}
				if existingPayment.PriceID != price.ID {
					return fmt.Errorf("duplicate renewal payment marker belongs to a different price")
				}
				if err := validateCompletedPayment(existingPayment, amount, currency); err != nil {
					return err
				}
				log.WithContext(ctx).WithFields(log.Fields{
					"transaction_id":  params.TransactionID,
					"subscription_id": subscription.ID,
					"rail":            params.Rail,
				}).Info("Renewal already processed; skipping duplicate lifecycle mutation")
				return nil
			}
		}

		// Calculate new billing period. A renewing subscription's price is
		// recurring; fall back to 30d (720h) if its cadence is somehow unset.
		cycleHours := BillingCycleHoursOf(price)
		if cycleHours <= 0 {
			// #651: a renewing subscription should carry a recurring cadence; if it
			// doesn't, warn instead of silently inventing 30d.
			log.WithContext(ctx).WithFields(log.Fields{
				"price_id":             price.ID,
				"rail_subscription_id": params.RailSubscriptionID,
			}).Warn("renewing price has no billing cadence; defaulting renewal period to 30d")
			cycleHours = 30 * 24
		}
		cycleWindow := time.Duration(cycleHours) * time.Hour
		var periodStartsAt, periodEndsAt time.Time
		if params.CurrentPeriodStartsAt != nil && !params.CurrentPeriodStartsAt.IsZero() {
			periodStartsAt = params.CurrentPeriodStartsAt.UTC()
			if params.CurrentPeriodEndsAt != nil && !params.CurrentPeriodEndsAt.IsZero() && params.CurrentPeriodEndsAt.After(periodStartsAt) {
				periodEndsAt = params.CurrentPeriodEndsAt.UTC()
			} else {
				periodEndsAt = periodStartsAt.Add(cycleWindow)
			}
		} else if subscription.CurrentPeriodEndsAt != nil && !subscription.CurrentPeriodEndsAt.IsZero() {
			periodStartsAt = *subscription.CurrentPeriodEndsAt
			periodEndsAt = periodStartsAt.Add(cycleWindow)
		} else {
			now := s.now()
			periodStartsAt = now
			periodEndsAt = now.Add(cycleWindow)
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
		subscription.ClearRetrySchedule()

		if err := subService.Update(ctx, subscription); err != nil {
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		renewalApplied = true
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":      subscription.ID,
			"user_id":              subscription.CustomerID.String(),
			"price_id":             price.ID,
			"rail":                 subscription.Rail,
			"rail_subscription_id": subscription.RailSubscriptionID,
			"period_start":         periodStartsAt,
			"period_end":           periodEndsAt,
			"downgrade_applied":    applyingDowngrade,
		}).Info("Updated subscription for renewal")

		// Append the next paid entitlement window for the subscription's entitlements.
		// Entitlement windows are immutable: renewals create new windows instead of extending existing ones.
		entitlementsSpec := subscription.EntitlementsSpecSnapshot
		if len(entitlementsSpec) == 0 {
			effectiveProduct := newProduct
			if effectiveProduct == nil {
				effectiveProduct, err = productService.GetByID(ctx, price.ProductID)
				if err != nil {
					return fmt.Errorf("failed to get product for renewal: %w", err)
				}
			}
			entitlementsSpec = effectiveProduct.EntitlementsSpec
		}
		if entitlementsSpec != nil {
			notBefore := periodStartsAt.UTC()
			endAt := periodEndsAt.UTC()
			// A renewal whose period is ALREADY entirely elapsed (a months-
			// stale backfill repair, e.g. the #367 liveness sync walking
			// charged-but-silent periods forward) grants no access window —
			// the period is over. Skipping the push (instead of letting
			// PushNewEntitlement error on the uncoverable past window) keeps
			// the payment backfill + period advance committing.
			grantWindows := endAt.After(s.now().UTC())
			if !grantWindows {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"period_end":      endAt,
				}).Warn("Renewal period already elapsed; advancing lifecycle without granting entitlement windows")
			}
			for entName := range entitlementsSpec {
				// If the subscription had rail-driven grace windows (e.g. CCBill dunning),
				// remove them before pushing the next paid window. Otherwise, the grace tail can
				// cause the paid push to be scheduled after grace or become a no-op.
				grace := models.EntitlementSourceGrace
				sid := subscription.ID
				if err := entitlementService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
					UserID:      subscription.CustomerID.String(),
					Entitlement: entName,
					SourceType:  &grace,
					SourceID:    &sid,
					Reason:      models.EntitlementRevokeSuperseded,
				}); err != nil {
					return fmt.Errorf("failed to clear grace entitlement %s on renewal: %w", entName, err)
				}

				if !grantWindows {
					continue
				}
				if _, err := entitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
					UserID:      subscription.CustomerID.String(),
					Entitlement: entName,
					NotBefore:   &notBefore,
					EndAt:       &endAt,
					SourceType:  models.EntitlementSourceSubscription,
					SourceID:    subscription.ID,
				}); err != nil {
					return fmt.Errorf("failed to grant renewal entitlement %s: %w", entName, err)
				}
			}

			// #368: the old period's grace was revoked above and the new paid
			// window pushed; pre-append the NEXT trailing grace window so the
			// next renewal's silence is bridged too.
			entNames := make([]string, 0, len(entitlementsSpec))
			for entName := range entitlementsSpec {
				entNames = append(entNames, entName)
			}
			s.appendRenewalGraceWindows(ctx, entitlementService, subscription, entNames, periodEndsAt, BillingCycleHoursOf(price))
		}

		// Handle entitlements for downgrade
		if applyingDowngrade && oldProduct != nil && newProduct != nil {
			// Determine which entitlements to revoke (in old but not in new)
			oldEnts := make(map[string]bool)
			if len(oldEntitlementsSpec) == 0 {
				oldEntitlementsSpec = oldProduct.EntitlementsSpec
			}
			for name := range oldEntitlementsSpec {
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
						UserID:      subscription.CustomerID.String(),
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
			ID:         uuidutil.NewV7(),
			CustomerID: subscription.CustomerID,
			EventType:  eventType,
			Data:       notifData,
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
		sub := &models.Subscription{ID: subscriptionID, CustomerID: identity.CustomerIDFromString(userID).UUID()}
		if err := s.EventLogService.LogLifecycleChargeSuccess(ctx, sub, params.Rail, params.TransactionID, params.Amount, params.Currency, s.now(), map[string]interface{}{"renewal": true}); err != nil {
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

	railSubID := strings.TrimSpace(params.RailSubscriptionID)
	if railSubID == "" {
		return nil, fmt.Errorf("rail subscription id is required")
	}

	now := s.now().UTC()
	if params.CurrentPeriodEndsAt == nil || params.CurrentPeriodEndsAt.IsZero() || !params.CurrentPeriodEndsAt.After(now) {
		return nil, fmt.Errorf("reactivation requires a future paid-through period end")
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"rail":                      params.Rail,
		"rail_subscription_id":      railSubID,
		"has_period_override":       params.CurrentPeriodEndsAt != nil,
		"allow_terminal_reactivate": params.AllowTerminalReactivation,
	}).Info("Starting membership reactivation flow")

	var reactivated *models.Subscription

	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(txdb)
		productService := catalog.NewProductService(txdb)
		subService := NewSubscriptionService(txdb, priceService, productService, nil, nil, nil, s.Clock())
		entitlementService := entitlements.NewEntitlementService(txdb, s.Clock())
		entitlementService.SetClock(s.Clock())

		subscription, err := subService.GetByRailSubscriptionID(ctx, string(params.Rail), railSubID)
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

		if len(subscription.EntitlementsSpecSnapshot) == 0 || len(subscription.CreditsSpecSnapshot) == 0 {
			product, err := productService.GetByID(ctx, price.ProductID)
			if err != nil {
				return fmt.Errorf("failed to load subscription product: %w", err)
			}
			if len(subscription.EntitlementsSpecSnapshot) == 0 {
				subscription.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(product.EntitlementsSpec)
			}
			if len(subscription.CreditsSpecSnapshot) == 0 {
				subscription.CreditsSpecSnapshot = models.CloneCreditsSpec(product.CreditsSpec)
			}
		}

		periodStartsAt := now
		periodEndsAt := params.CurrentPeriodEndsAt.UTC()

		subscription.Status = models.StatusActive
		subscription.CurrentPeriodStartsAt = &periodStartsAt
		subscription.CurrentPeriodEndsAt = &periodEndsAt
		subscription.CancelledAt = nil
		subscription.CancelType = nil
		subscription.CancelFeedback = nil
		subscription.EndedAt = nil
		subscription.ClearRetrySchedule()

		if err := subService.Update(ctx, subscription); err != nil {
			return fmt.Errorf("failed to update reactivated subscription: %w", err)
		}

		entNames := make([]string, 0)
		entitlementsSpec := subscription.EntitlementsSpecSnapshot
		if len(entitlementsSpec) > 0 {
			entNames = make([]string, 0, len(entitlementsSpec))
			for name := range entitlementsSpec {
				entNames = append(entNames, name)
			}
		} else {
			// #651: don't fabricate a "premium" entitlement on reactivation; restore
			// only what the subscription snapshot declares (here: nothing) and warn.
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.CustomerID.String(),
			}).Warn("reactivated subscription declares no entitlements; restoring none (was fabricating \"premium\")")
		}

		notBefore := periodStartsAt.UTC()
		endAt := periodEndsAt.UTC()
		graceSource := models.EntitlementSourceGrace
		subSource := models.EntitlementSourceSubscription
		subID := subscription.ID

		for _, entName := range entNames {
			if err := entitlementService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
				UserID:      subscription.CustomerID.String(),
				Entitlement: entName,
				SourceType:  &graceSource,
				SourceID:    &subID,
				Reason:      models.EntitlementRevokeSuperseded,
			}); err != nil {
				return fmt.Errorf("failed to clear grace entitlement %s on reactivation: %w", entName, err)
			}

			if _, err := entitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
				UserID:      subscription.CustomerID.String(),
				Entitlement: entName,
				NotBefore:   &notBefore,
				EndAt:       &endAt,
				SourceType:  subSource,
				SourceID:    subID,
			}); err != nil {
				return fmt.Errorf("failed to restore entitlement %s on reactivation: %w", entName, err)
			}
		}

		// #368: a reactivated membership renews like any other — pre-append
		// the trailing grace window behind the restored paid window.
		s.appendRenewalGraceWindows(ctx, entitlementService, subscription, entNames, periodEndsAt, BillingCycleHoursOf(price))

		reactivated = subscription
		return nil
	})
	if err != nil {
		return nil, err
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":        reactivated.ID,
		"user_id":                reactivated.CustomerID.String(),
		"rail":                   reactivated.Rail,
		"rail_subscription_id":   reactivated.RailSubscriptionID,
		"current_period_ends_at": reactivated.CurrentPeriodEndsAt,
	}).Info("Membership reactivation flow completed")

	return reactivated, nil
}

// CancelMembership cancels a subscription and revokes associated roles
func (s *SubscriptionLifecycleService) CancelMembership(ctx context.Context, params *CancelMembershipParams) error {
	notifications := make([]*models.NotificationQueue, 0, 1)

	// Variables to capture from transaction for event logging
	var subscriptionID uuid.UUID
	var userID string
	var rail models.Rail

	var procName string
	if params.Rail != nil {
		procName = string(*params.Rail)
	}
	procSub := normalize.FromPtr(params.RailSubscriptionID)
	subID := ""
	if params.SubscriptionID != nil {
		subID = params.SubscriptionID.String()
	}
	cancelFeedback := normalize.FromPtr(params.CancelFeedback)
	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":           subID,
		"rail":                      procName,
		"rail_subscription_id":      procSub,
		"cancel_type":               params.CancelType,
		"revoke_access_immediately": params.RevokeAccess,
		"cancel_feedback_provided":  cancelFeedback != "",
	}).Info("Starting membership cancellation flow")

	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		db := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(db)
		productService := catalog.NewProductService(db)
		notificationRepo := repo.NewNotificationQueueRepo(db)
		subService := NewSubscriptionService(db, priceService, productService, nil, nil, nil, s.Clock())

		// Use rail name for gateway lookup
		// Find subscription
		var subscription *models.Subscription
		var err error

		if params.SubscriptionID != nil {
			subscription, err = subService.GetByID(ctx, *params.SubscriptionID)
		} else if params.RailSubscriptionID != nil && params.Rail != nil {
			subscription, err = subService.GetByRailSubscriptionID(ctx, string(*params.Rail), *params.RailSubscriptionID)
		} else {
			return fmt.Errorf("either subscription_id or rail details must be provided")
		}

		if err != nil {
			log.WithContext(ctx).WithError(err).Warn("Failed to locate subscription for cancellation")
			return fmt.Errorf("subscription not found: %w", err)
		}

		// Capture values for event logging after transaction
		subscriptionID = subscription.ID
		userID = subscription.CustomerID.String()
		rail = subscription.Rail

		// Cancellation policy (caller-owned): an immediate revoke truncates the
		// paid period to now; a period-end cancel keeps paid access until the term
		// ends and only forfeits the pre-appended #368 grace window. The terminal
		// status flip + Solana cascade + entitlement revoke are the shared local-
		// state core (ApplyLocalCancellation), so this path can never diverge from
		// the LIFE-plane convergence repairs.
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

		// Immediate (revoke now / already-ended period): revoke the subscription's
		// paid windows AND any scheduled grace. Period-end: forfeit only the
		// scheduled grace window (#368), keep paid access until term end.
		immediate := params.RevokeAccess || subscription.CurrentPeriodEndsAt == nil || !subscription.CurrentPeriodEndsAt.After(now)
		revokeReason := models.EntitlementRevokeAdmin
		revokeSources := []models.EntitlementSourceType{models.EntitlementSourceGrace}
		if immediate {
			if params.CancelType == models.CancelTypeChargeback {
				revokeReason = models.EntitlementRevokeChargeback
			}
			revokeSources = []models.EntitlementSourceType{models.EntitlementSourceSubscription, models.EntitlementSourceGrace}
		}

		if err := s.ApplyLocalCancellation(ctx, db, subscription, LocalCancellation{
			EndedAt:       endAt,
			CancelType:    params.CancelType,
			Feedback:      params.CancelFeedback,
			RevokeReason:  revokeReason,
			RevokeAsOf:    now,
			RevokeSources: revokeSources,
		}); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscription.ID,
			}).Error("Failed to apply local cancellation")
			return err
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"user_id":         subscription.CustomerID.String(),
			"status":          subscription.Status,
			"ended_at":        subscription.EndedAt,
			"period_end":      subscription.CurrentPeriodEndsAt,
		}).Info("Updated subscription record during cancellation")

		reason := PremiumEndReasonAdmin
		switch params.CancelType {
		case models.CancelTypeUser:
			reason = PremiumEndReasonUserCancel
		case models.CancelTypeExpired:
			reason = PremiumEndReasonExpired
		case models.CancelTypeMerchant:
			reason = PremiumEndReasonRail
		}

		notification := &models.NotificationQueue{
			ID:         uuidutil.NewV7(),
			CustomerID: subscription.CustomerID,
			EventType:  models.NotificationPremiumEnded,
			Data:       map[string]any{"reason": string(reason)},
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
		if err := s.EventLogService.LogLifecycleCancellation(ctx, subscriptionID, userID, rail, params.CancelType, params.RevokeAccess, s.now()); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscriptionID,
				"event_type":      "subscription_cancelled",
			}).Warn("failed to log subscription cancelled event to ClickHouse")
		}
	}

	return nil
}

// LocalCancellation describes a side-effect-free terminal cancellation of a
// subscription — the local-state transition shared by the user-driven
// CancelMembership path and the LIFE-plane convergence repairs (grace_exhausted /
// pending_stale).
type LocalCancellation struct {
	EndedAt       time.Time                      // subscriptions.ended_at
	CancelType    models.CancelType              // subscriptions.cancel_type
	Feedback      *string                        // subscriptions.cancel_feedback
	RevokeReason  models.EntitlementRevokeReason // entitlement revoke_reason (used only when RevokeSources non-empty)
	RevokeAsOf    time.Time                      // instant entitlements are revoked as-of (converge-not-replay)
	RevokeSources []models.EntitlementSourceType // entitlement sources to revoke; empty = revoke nothing
}

// ApplyLocalCancellation performs the side-effect-free LOCAL-STATE transition of
// cancelling `sub`: the terminal status flip + ended/cancel fields + cleared
// retry/grace schedule, the #264 Solana cranker cascade, and revocation of the
// named entitlement sources as-of RevokeAsOf. It deliberately does NOT send
// notifications, write the lifecycle event log, or enqueue provider intents —
// those durable side-effects belong to the caller (CancelMembership layers them
// on after this returns).
//
// It runs every write on the supplied `dbb`, so the CALLER owns atomicity:
// CancelMembership passes its MerchantTx-bound handle (all writes commit
// together); the convergence engine passes its merchant-scoped connection (the
// idempotent sweep heals any partial write). This is the single chokepoint where
// the local outcome of "cancel a subscription" is defined — the converged path
// can no longer diverge from the user path (notably: the Solana cascade, whose
// absence previously left a converged Solana cancel pulling forever).
//
// The caller loads `sub` (and may pre-adjust its period bounds — e.g. truncate
// to now for an immediate revoke) before calling; this method owns only the
// terminal status/cancel fields + cascade + revoke.
func (s *SubscriptionLifecycleService) ApplyLocalCancellation(ctx context.Context, dbb *db.DB, sub *models.Subscription, c LocalCancellation) error {
	if dbb == nil || sub == nil {
		return fmt.Errorf("apply local cancellation: db handle and subscription are required")
	}
	now := s.now()
	endedAt := c.EndedAt
	// cancelled_at is the operation instant, but never after ended_at: the
	// chk_ended_not_before_cancelled constraint requires ended_at >= cancelled_at,
	// and an immediate revoke pins ended_at to the caller's `now` (computed a hair
	// before this method's own s.now()).
	cancelledAt := now
	if endedAt.Before(cancelledAt) {
		cancelledAt = endedAt
	}
	cancelType := c.CancelType
	sub.Status = models.StatusCancelled
	sub.EndedAt = &endedAt
	sub.CancelType = &cancelType
	sub.CancelFeedback = c.Feedback
	sub.CancelledAt = &cancelledAt
	sub.ClearRetrySchedule()

	if err := repo.NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, now); err != nil {
		return fmt.Errorf("apply local cancellation: update subscription %s: %w", sub.ID, err)
	}

	// #264 cascade: stop the Solana cranker. Log-and-continue (never fail the
	// cancel on the cascade), matching the user path; idempotent + tolerant of a
	// missing solana_subscriptions row.
	if sub.Rail == models.RailSolana {
		if err := cancelSolanaSubscriptionCascade(ctx, dbb, sub.ID); err != nil {
			log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).
				Error("failed to cascade cancellation to solana_subscriptions row; cranker may keep pulling")
		}
	}

	if len(c.RevokeSources) > 0 {
		entSvc := entitlements.NewEntitlementService(dbb, s.Clock())
		entSvc.SetClock(s.Clock())
		if err := entSvc.RevokeSourcesForSubscriptionAsOf(ctx, sub.CustomerID.String(), sub.ID, c.RevokeAsOf, c.RevokeReason, c.RevokeSources...); err != nil {
			log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).
				Error("failed to revoke entitlements during local cancellation")
		}
	}
	return nil
}

// ApplyLocalPastDue performs the side-effect-free LOCAL-STATE transition of an
// active subscription into dunning (past_due) with a grace window dated to the
// supplied instant (the missed period end, not wall-clock — so a long-overdue
// sub's grace is already exhausted and the grace_exhausted repair terminates it
// on the next pass; converge-not-replay, no missed charges re-run). The grace
// window is set only when none exists (COALESCE semantics). No-op unless the sub
// is currently active. Like ApplyLocalCancellation, it runs on the supplied
// `dbb` so the caller owns atomicity; the LIFE-plane period_overdue repair is
// the sole caller today.
func (s *SubscriptionLifecycleService) ApplyLocalPastDue(ctx context.Context, dbb *db.DB, sub *models.Subscription, graceEndsAt time.Time) error {
	if dbb == nil || sub == nil {
		return fmt.Errorf("apply local past_due: db handle and subscription are required")
	}
	if sub.Status != models.StatusActive {
		return nil // idempotent: only an active sub enters dunning here
	}
	sub.Status = models.StatusPastDue
	if sub.GraceEndsAt == nil {
		ge := graceEndsAt
		sub.GraceEndsAt = &ge
	}
	if err := repo.NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, s.now()); err != nil {
		return fmt.Errorf("apply local past_due: update subscription %s: %w", sub.ID, err)
	}
	return nil
}

// ApplyLocalUnknown is the side-effect-free LOCAL transition of an active,
// period-elapsed, provider-auto-billed subscription into `unknown` (#632): a
// needs-provider-verification holding state. Convergence will NOT guess whether the
// provider billed it — provider-pull (#633) resolves it via ResolveUnknownSubscription.
// No-op unless the sub is currently active (idempotent). Access (the entitlement
// window) is intentionally LEFT INTACT while unknown — we do not revoke on a guess;
// only a confirmed provider outcome (cancel) revokes.
func (s *SubscriptionLifecycleService) ApplyLocalUnknown(ctx context.Context, dbb *db.DB, sub *models.Subscription) error {
	if dbb == nil || sub == nil {
		return fmt.Errorf("apply local unknown: db handle and subscription are required")
	}
	if sub.Status != models.StatusActive {
		return nil // idempotent: only an active sub enters verification limbo
	}
	sub.Status = models.StatusUnknown
	if err := repo.NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, s.now()); err != nil {
		return fmt.Errorf("apply local unknown: update subscription %s: %w", sub.ID, err)
	}
	return nil
}

// UnknownResolution is the provider-confirmed outcome for an `unknown` subscription,
// produced by the #633 batched provider-pull and applied by ResolveUnknownSubscription.
type UnknownResolution int

const (
	// ResolveUnreachable: the provider was not reachable (no creds / down / rate
	// limited). The sub STAYS unknown and is retried with exponential backoff (#633).
	ResolveUnreachable UnknownResolution = iota
	// ResolveRenewed: the provider confirms a current/renewed billing relationship
	// (a charge landed for the new period, or the remote sub is active). Advance the
	// local period to the provider's period end and return to `active`.
	ResolveRenewed
	// ResolvePastDue: the provider confirms the renewal payment FAILED but the sub is
	// still recoverable within the dunning window. Enter `past_due` so dunning/grace
	// runs (an our-rebill sub) or grace_exhausted terminates it.
	ResolvePastDue
	// ResolveCancelled: the provider deleted/cancelled the remote subscription.
	// Terminal: cancel locally and revoke the access window as-of the period end.
	ResolveCancelled
)

// ResolveUnknownSubscription applies a provider-confirmed outcome to an `unknown`
// subscription (#632). Side-effect-free local-state transition on the supplied
// `dbb` (the caller owns atomicity), mirroring ApplyLocalPastDue/Cancellation.
// No-op unless the sub is currently `unknown` (idempotent — a concurrent resolve or
// a re-run of the same pull lands the same state once). newPeriodEnd is the
// provider's confirmed period end (used by ResolveRenewed); graceEndsAt dates the
// dunning grace window (ResolvePastDue), normally the missed period end.
func (s *SubscriptionLifecycleService) ResolveUnknownSubscription(ctx context.Context, dbb *db.DB, sub *models.Subscription, res UnknownResolution, newPeriodEnd *time.Time, graceEndsAt time.Time) error {
	if dbb == nil || sub == nil {
		return fmt.Errorf("resolve unknown: db handle and subscription are required")
	}
	if sub.Status != models.StatusUnknown {
		return nil // idempotent
	}
	now := s.now()
	switch res {
	case ResolveUnreachable:
		return nil // stay unknown; #633 retries with backoff
	case ResolveRenewed:
		sub.Status = models.StatusActive
		if newPeriodEnd != nil {
			// New period starts at the prior period end (or now if unknown), ends at
			// the provider-confirmed end. The renewal payment is backfilled by #634.
			start := now
			if sub.CurrentPeriodEndsAt != nil {
				start = *sub.CurrentPeriodEndsAt
			}
			if newPeriodEnd.After(start) {
				sub.CurrentPeriodStartsAt = &start
				end := *newPeriodEnd
				sub.CurrentPeriodEndsAt = &end
			}
		}
		sub.ClearRetrySchedule()
		if err := repo.NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, now); err != nil {
			return fmt.Errorf("resolve unknown (renewed) %s: %w", sub.ID, err)
		}
		return nil
	case ResolvePastDue:
		sub.Status = models.StatusPastDue
		if sub.GraceEndsAt == nil {
			ge := graceEndsAt
			sub.GraceEndsAt = &ge
		}
		if err := repo.NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, now); err != nil {
			return fmt.Errorf("resolve unknown (past_due) %s: %w", sub.ID, err)
		}
		return nil
	case ResolveCancelled:
		asOf := now
		if sub.CurrentPeriodEndsAt != nil {
			asOf = *sub.CurrentPeriodEndsAt
		}
		fb := "cancelled at provider (converged from unknown)"
		return s.ApplyLocalCancellation(ctx, dbb, sub, LocalCancellation{
			EndedAt:       now,
			CancelType:    models.CancelTypeExpired,
			Feedback:      &fb,
			RevokeReason:  models.EntitlementRevokeDunning,
			RevokeAsOf:    asOf,
			RevokeSources: []models.EntitlementSourceType{models.EntitlementSourceSubscription, models.EntitlementSourceGrace},
		})
	default:
		return fmt.Errorf("resolve unknown: unknown resolution %d", res)
	}
}

// cancelSolanaSubscriptionCascade flips the linked openrails.solana_subscriptions
// row to cancelled so the hourly Solana cranker's ListDue (which filters
// status = active) no longer returns it — billing stops because OpenRails is the
// only puller (#264). `d` must be the tx-bound db handle so the cascade commits
// atomically with the lifecycle cancellation. Idempotent: setting an
// already-cancelled row to cancelled is a no-op. Tolerant of a missing row (a
// Solana sub that was never enrolled): returns nil after logging so the cancel
// itself never fails on the cascade.
func cancelSolanaSubscriptionCascade(ctx context.Context, d *db.DB, subscriptionID uuid.UUID) error {
	solanaRepo := repo.NewSolanaSubscriptionRepo(d)
	row, err := solanaRepo.GetBySubscriptionID(ctx, subscriptionID)
	if err != nil {
		if repo.IsNotFound(err) {
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscriptionID,
			}).Info("no solana_subscriptions row linked to cancelled subscription; nothing to cascade")
			return nil
		}
		return fmt.Errorf("lookup solana_subscriptions row: %w", err)
	}
	if row.Status == models.SolanaSubscriptionCancelled {
		return nil
	}
	if err := solanaRepo.SetStatus(ctx, row.ID, models.SolanaSubscriptionCancelled); err != nil {
		return fmt.Errorf("set solana_subscriptions status cancelled: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":        subscriptionID,
		"solana_subscription_id": row.ID,
	}).Info("cascaded cancellation to solana_subscriptions row; cranker stopped")
	return nil
}

// ExpireMembership expires a subscription and revokes associated roles
func (s *SubscriptionLifecycleService) ExpireMembership(ctx context.Context, subscriptionID uuid.UUID) error {
	notifications := make([]*models.NotificationQueue, 0, 1)

	log.WithContext(ctx).WithField("subscription_id", subscriptionID).Info("Starting membership expiration flow")

	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		db := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(db)
		productService := catalog.NewProductService(db)
		notificationRepo := repo.NewNotificationQueueRepo(db)
		subService := NewSubscriptionService(db, priceService, productService, nil, nil, nil, s.Clock())
		entSvc := entitlements.NewEntitlementService(db, s.Clock())
		entSvc.SetClock(s.Clock()) // Propagate clock for testing

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
		subscription.ClearRetrySchedule()

		if err := subService.Update(ctx, subscription); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscription.ID,
			}).Error("Failed to update subscription during expiration")
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"user_id":         subscription.CustomerID.String(),
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
						UserID:      subscription.CustomerID.String(),
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
						UserID:      subscription.CustomerID.String(),
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
			ID:         uuidutil.NewV7(),
			CustomerID: subscription.CustomerID,
			EventType:  models.NotificationPremiumEnded,
			Data:       map[string]any{"reason": string(PremiumEndReasonExpired)},
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
func (s *SubscriptionLifecycleService) FailMembership(ctx context.Context, params *FailMembershipParams) error {
	if params == nil || params.SubscriptionID == nil || *params.SubscriptionID == uuid.Nil {
		return fmt.Errorf("subscription_id is required")
	}

	notifications := make([]*models.NotificationQueue, 0, 1)

	// Variables to capture from transaction for event logging
	var subscriptionID uuid.UUID
	var userID string
	var finalStatus models.SubscriptionStatus
	// Set inside the tx when a terminal cancellation must also stop the
	// remote NMI recurring subscription; the job is enqueued after commit.
	var scheduleDeferredDelete bool

	log.WithContext(ctx).WithFields(log.Fields{
		"rail":                 params.Rail,
		"rail_subscription_id": params.SubscriptionID,
		"failure_reason":       normalize.FromPtr(params.FailureReason),
		"failure_code":         normalize.FromPtr(params.FailureCode),
	}).Warn("Starting membership failure flow")

	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		db := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(db)
		productService := catalog.NewProductService(db)
		notificationRepo := repo.NewNotificationQueueRepo(db)
		subService := NewSubscriptionService(db, priceService, productService, nil, nil, nil, s.Clock())
		entSvc := entitlements.NewEntitlementService(db, s.Clock())
		entSvc.SetClock(s.Clock()) // Propagate clock for testing

		subscription, err := subService.GetByID(ctx, *params.SubscriptionID)
		if err != nil {
			log.WithContext(ctx).WithError(err).Warn("Failed to locate subscription for failure flow")
			return fmt.Errorf("subscription not found: %w", err)
		}

		// Capture values for event logging
		subscriptionID = subscription.ID
		userID = subscription.CustomerID.String()
		scheduleDeferredDelete = false // reset in case the tx is retried

		now := s.now()

		// Hard declines (stolen card, do-not-honor, account closed, expired card,
		// pickup card) terminate immediately: cancel now with no grace period and
		// no further retry scheduling. Retrying a hard decline cannot succeed and
		// risks flagging the merchant with the card networks.
		if params.HardDecline {
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.CustomerID,
				"failure_code":    normalize.FromPtr(params.FailureCode),
			}).Warn("Hard decline received; immediately cancelling subscription (no retry)")
			expired := models.CancelTypeExpired
			reason := normalize.FromPtr(params.FailureReason)
			if reason == "" {
				reason = "transaction_failure"
			}
			subscription.Status = models.StatusCancelled
			subscription.CancelledAt = &now
			subscription.CancelType = &expired
			subscription.CancelFeedback = &reason
			subscription.EndedAt = &now
			subscription.ClearRetrySchedule()
		} else {
			// Update subscription status - failed payment = past_due (still trying to recover)
			subscription.Status = models.StatusPastDue

			// #359: the dunning cadence is a hardcoded function of the price's
			// billing cycle (monthly: 5 failures total, progressive retries at
			// +2d/+5d/+9d/+13d; weekly-ish: retries at +1d/+2d; daily-ish: the
			// first failure is terminal). See DunningRetryOffsets.
			cycleHours := BillingCycleHoursOf(subscription.Price)
			if cycleHours <= 0 {
				if price, perr := priceService.GetByID(ctx, subscription.PriceID); perr == nil {
					cycleHours = BillingCycleHoursOf(price)
				}
			}
			if cycleHours <= 0 {
				// A past_due subscription on a one-time price shouldn't exist;
				// defensively dun it on the monthly schedule.
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"price_id":        subscription.PriceID,
				}).Warn("FailMembership: subscription has no billing cycle (one-time price?); using monthly dunning schedule")
			}
			maxFailures := DunningMaxFailures(cycleHours)

			terminal := params.Terminal
			if !terminal {
				subscription.LastRetryAt = &now
				if subscription.RetryAttempts == nil {
					attempts := 1
					subscription.RetryAttempts = &attempts
				} else {
					*subscription.RetryAttempts++
				}
				// maxFailures == 1 (sub-4-day cycles) makes the FIRST failure
				// terminal: straight to cancel + revoke + scheduled NMI delete.
				terminal = *subscription.RetryAttempts >= maxFailures
			}

			// Terminal (window expired, or the schedule's max failures reached):
			// cancel; otherwise schedule the next retry at the schedule's gap
			// for this failure count (relative to now, so a late worker run
			// never schedules into the past)
			if terminal {
				expired := models.CancelTypeExpired
				reason := normalize.FromPtr(params.FailureReason)
				if reason == "" {
					reason = "transaction_failure"
				}
				subscription.Status = models.StatusCancelled
				subscription.CancelledAt = &now
				subscription.CancelType = &expired
				subscription.CancelFeedback = &reason
				subscription.EndedAt = &now
				subscription.ClearRetrySchedule()
			} else {
				nextRetry := now.Add(DunningNextRetryIn(cycleHours, *subscription.RetryAttempts))
				subscription.NextRetryAt = &nextRetry
			}
		}

		// For OpenRails-driven rails (NMI-backed + Solana), we control retry
		// timing; if the retry schedule would extend beyond the paid term end, model
		// that access as explicit grace entitlement windows. (#257: Solana gets the
		// same paid-through grace as NMI.)
		if rails.OpenRailsDrivenDunning(subscription.Rail) && subscription.Status == models.StatusPastDue {
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
							UserID:      subscription.CustomerID.String(),
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

		// #344 follow-up: a terminal payment-failure cancellation of an
		// NMI-backed subscription must also stop the rail-side recurring
		// subscription, or NMI keeps retrying it monthly forever. The
		// DeletionScheduledAt marker AND the nmi_delete intent are both
		// written inside this transaction (atomic — no crash window between
		// marker and intent). The intent executor plus the #344 client-level
		// kill switch then govern actual execution.
		if subscription.Status == models.StatusCancelled &&
			rails.IsNMI(subscription.Rail) &&
			subscription.RailSubscriptionID != "" {
			if s.Config != nil && s.Config.IsLimitedMode() {
				// Limited mode (#345): no proactive provider action — leave the
				// remote subscription for reconciliation.
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id":      subscription.ID,
					"rail":                 subscription.Rail,
					"rail_subscription_id": subscription.RailSubscriptionID,
				}).Warn("Limited mode: remote rail subscription left alive for reconciliation (no proactive provider action)")
			} else if s.deferDelete != nil {
				subscription.DeletionScheduledAt = &now
				scheduleDeferredDelete = true
			}
		}

		if err := subService.Update(ctx, subscription); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscription.ID,
			}).Error("Failed to update subscription during failure flow")
			return fmt.Errorf("failed to update subscription: %w", err)
		}

		// Terminal cancellation with a remote NMI schedule: enqueue the
		// deferred delete intent IN THIS TRANSACTION so the
		// DeletionScheduledAt marker and the intent commit atomically (no
		// crash window between them).
		if scheduleDeferredDelete {
			if err := s.deferDelete.WithTx(tx).ScheduleNMIDelete(ctx, subscription.CustomerID.String(), subscription.ID, now); err != nil {
				return fmt.Errorf("enqueue deferred NMI delete with cancellation: %w", err)
			}
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscription.ID,
			"user_id":         subscription.CustomerID.String(),
			"status":          subscription.Status,
			"retry_attempts":  subscription.RetryAttempts,
			"next_retry_at":   subscription.NextRetryAt,
		}).Warn("Updated subscription during failure flow")

		// Capture final status for event logging
		finalStatus = subscription.Status

		// Revoke entitlements if subscription is cancelled after max retries or a
		// terminal decline.
		if subscription.Status == models.StatusCancelled && entSvc != nil {
			names, err := entSvc.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceSubscription, subscription.ID)
			if err != nil {
				log.WithContext(ctx).WithError(err).Error("failed to list entitlements for failed subscription")
			} else {
				st := models.EntitlementSourceSubscription
				sid := subscription.ID
				for _, entName := range names {
					if err := entSvc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
						UserID:      subscription.CustomerID.String(),
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
						UserID:      subscription.CustomerID.String(),
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

		eventType := models.NotificationPaymentMethodFailed
		if subscription.Status == models.StatusCancelled {
			eventType = models.NotificationPremiumEnded
		}

		var data map[string]any
		if eventType == models.NotificationPremiumEnded {
			data = map[string]any{"reason": string(PremiumEndReasonExpired)}
		}

		notification := &models.NotificationQueue{
			ID:         uuidutil.NewV7(),
			CustomerID: subscription.CustomerID,
			EventType:  eventType,
			Data:       data,
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

	// Schedule the deferred NMI delete AFTER the cancellation committed. Runs
	// at "now": the undo-window semantics of user cancellations do not apply to
	// dunning exhaustion. Idempotent via the intent ledger's idempotency_key
	// (#358). On failure the persisted DeletionScheduledAt marker keeps the
	// pending delete discoverable (startup marker sweep / #107
	// reconciliation), so we log instead of failing the flow.
	if scheduleDeferredDelete {
		// Enqueued inside the failure-flow transaction above; this is just
		// the operator-visible confirmation.
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscriptionID,
			"user_id":         userID,
		}).Info("scheduled deferred NMI delete after terminal payment failure (committed with the cancellation)")
	}

	s.dispatchNotifications(ctx, notifications)

	// Log the payment failure event to ClickHouse
	if s.EventLogService != nil {
		if err := s.EventLogService.LogLifecycleFailure(ctx, subscriptionID, userID, params.Rail, finalStatus, params.FailureReason, params.FailureCode, s.now()); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscriptionID,
				"event_type":      "charge_failure",
			}).Warn("failed to log payment failure event to ClickHouse")
		}
	}

	return nil
}

func (s *SubscriptionLifecycleService) logPaymentEvent(ctx context.Context, sub *models.Subscription, rail models.Rail, transactionID string, amount int64, currency string) {
	if s.EventLogService == nil || sub == nil {
		return
	}
	if err := s.EventLogService.LogLifecycleChargeSuccess(ctx, sub, rail, transactionID, amount, currency, s.now(), nil); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"subscription_id": sub.ID,
			"event_type":      "charge_success",
			"rail":            rail,
		}).Warn("failed to log payment event to ClickHouse")
	}
}

func validateCompletedPayment(payment *models.Payment, expectedAmount int64, expectedCurrency string) error {
	if payment == nil {
		return errors.New("payment is required")
	}
	status := strings.TrimSpace(payment.Status)
	if status != "" && !strings.EqualFold(status, "completed") {
		return fmt.Errorf("payment transaction is not completed")
	}
	if expectedAmount > 0 && payment.Amount != expectedAmount {
		return fmt.Errorf("payment transaction amount mismatch")
	}
	if expectedCurrency != "" && !strings.EqualFold(strings.TrimSpace(payment.Currency), strings.TrimSpace(expectedCurrency)) {
		return fmt.Errorf("payment transaction currency mismatch")
	}
	return nil
}

// Parameter structs for lifecycle operations
