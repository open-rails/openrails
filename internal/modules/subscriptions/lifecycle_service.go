package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
	"reflect"
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
	NotificationService *NotificationService
	PaymentService      *payments.PaymentService // For creating Payment records on renewal

	// These seams keep terminal state changes and their access side effects
	// testable as one transaction without changing the public service API.
	entitlementServiceFactory func(*db.DB, clockwork.Clock) lifecycleEntitlementService
	cancelSolanaSubscription  func(context.Context, *db.DB, uuid.UUID) error

	// deferDelete enqueues the deferred NMI delete_subscription job (#344
	// follow-up). Optional: injected via SetDeferredDeleteScheduler in the
	// composition root (same pattern as UserSubscriptionService.deferDelete).
	// When nil, terminal dunning cancellations leave the remote NMI
	// subscription alive (caller-side paths or #107 reconciliation handle it).
	deferDelete DeferredDeleteScheduler
}

type lifecycleEntitlementService interface {
	ListDistinctEntitlementNamesBySource(context.Context, models.EntitlementSourceType, uuid.UUID) ([]string, error)
	PushNewEntitlement(context.Context, entitlements.PushNewEntitlementParams) (*models.Entitlement, error)
	RevokeExistingEntitlement(context.Context, entitlements.RevokeExistingEntitlementParams) error
	RevokeSourcesForSubscriptionAsOf(context.Context, string, uuid.UUID, time.Time, models.EntitlementRevokeReason, ...models.EntitlementSourceType) error
	BoundSubscriptionAccess(context.Context, uuid.UUID, time.Time) error
	ResumeSubscriptionAccess(context.Context, uuid.UUID) error
}

func (s *SubscriptionLifecycleService) newLifecycleEntitlementService(dbb *db.DB) lifecycleEntitlementService {
	if s.entitlementServiceFactory != nil {
		return s.entitlementServiceFactory(dbb, s.Clock())
	}
	entSvc := entitlements.NewEntitlementService(dbb, s.Clock())
	entSvc.SetClock(s.Clock())
	return entSvc
}

func (s *SubscriptionLifecycleService) cancelSolanaSubscriptionForLifecycle(ctx context.Context, dbb *db.DB, subscriptionID uuid.UUID) error {
	if s.cancelSolanaSubscription != nil {
		return s.cancelSolanaSubscription(ctx, dbb, subscriptionID)
	}
	return cancelSolanaSubscriptionCascade(ctx, dbb, subscriptionID)
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
func NewSubscriptionLifecycleService(db *db.DB, productService *catalog.ProductService, priceService *catalog.PriceService, entitlementService *entitlements.EntitlementService, notificationService *NotificationService, paymentService *payments.PaymentService, clocks ...clockwork.Clock) *SubscriptionLifecycleService {
	return &SubscriptionLifecycleService{
		DB:                  db,
		Config:              nil,                            // Set via SetConfig if feature flags are needed
		clock:               timeutil.FirstClock(clocks...), // Default to real clock, can be overridden for tests
		ProductService:      productService,
		PriceService:        priceService,
		EntitlementService:  entitlementService,
		NotificationService: notificationService,
		PaymentService:      paymentService,
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

// SetCreditGranter installs the transaction-aware subscription credit writer.
// A credit-bearing lifecycle fails closed when this dependency is absent.

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

// (#368 appendRenewalGraceWindows deleted by #691: auto-renew subscription
// windows are STANDING — silence at period end cannot cut access, so no grace
// window is pre-appended. Historical `grace` rows are still revoked on
// renewal/cancel/terminal paths.)

// DispatchNotifications delivers notification rows after their surrounding
// transaction commits. Transaction-aware lifecycle callers use this to avoid
// sending messages for work that may still roll back.
func (s *SubscriptionLifecycleService) DispatchNotifications(ctx context.Context, notifications []*models.NotificationQueue) {
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
		subscription, notifications, err = s.CreateMembershipTx(ctx, dbb, params)
		return err
	})
	if err != nil {
		return nil, err
	}

	s.DispatchNotifications(ctx, notifications)

	return subscription, nil
}

// CreateMembershipTx executes the membership creation logic using the provided transactional DB.
// The caller is responsible for wrapping the call in a transaction and dispatching any queued notifications.
func (s *SubscriptionLifecycleService) CreateMembershipTx(ctx context.Context, txDB *db.DB, params *CreateMembershipParams) (*models.Subscription, []*models.NotificationQueue, error) {
	if txDB == nil {
		return nil, nil, errors.New("transaction DB is required")
	}
	if params == nil || (params.InitialPaymentReversal != "" && params.Prepared == nil) {
		return nil, nil, errors.New("initial reversal requires accepted terms")
	}
	if params.Prepared != nil {
		terms := params.Prepared
		if txDB.Pool() != nil {
			return nil, nil, errors.New("accepted membership requires a transaction")
		}
		if err := terms.Validate(); err != nil {
			return nil, nil, err
		}
		if params.InitialPaymentReversal != "" && (terms.CollectionPolicy != models.CollectionPolicyEngine || terms.Amount <= 0 || params.Rail != models.RailStripe || (params.InitialPaymentReversal != "refund" && params.InitialPaymentReversal != "dispute")) {
			return nil, nil, errors.New("invalid accepted initial payment reversal")
		}
		if (terms.Amount > 0 && s.PaymentService == nil) || params.UserID != terms.CustomerID.String() || params.PriceID != terms.PriceID || db.PSPIDFromContext(ctx) != terms.PSPID || (terms.Amount > 0) != (strings.TrimSpace(params.TransactionID) != "") || (terms.CollectionPolicy != models.CollectionPolicyEngine && (params.RailSubscriptionID == nil || strings.TrimSpace(*params.RailSubscriptionID) == "")) || (terms.CollectionPolicy == models.CollectionPolicyEngine && params.RailSubscriptionID != nil && strings.TrimSpace(*params.RailSubscriptionID) != "") {
			return nil, nil, errors.New("membership completion contradicts accepted terms")
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, nil, err
		}
		prior, err := txDB.Gen(ctx).GetInitialMembershipForUpdate(ctx, gen.GetInitialMembershipForUpdateParams{MerchantID: mid.UUID(), ID: terms.SubscriptionID})
		if err != nil && !db.IsNotFound(err) {
			return nil, nil, err
		}
		if err == nil {
			sub, err := models.SubscriptionFromGen(prior)
			if err != nil {
				return nil, nil, err
			}
			provider := ""
			if params.RailSubscriptionID != nil {
				provider = strings.TrimSpace(*params.RailSubscriptionID)
			}
			if err := terms.ValidateSubscriptionIdentity(sub, params.Rail, provider); err != nil {
				return nil, nil, err
			}
			if sub.Status != models.StatusPending || terms.Pending {
				if terms.Amount > 0 {
					payment, err := payments.NewPaymentRepo(txDB).GetByID(ctx, terms.PaymentID)
					if err != nil {
						return nil, nil, err
					}
					if err := ValidateInitialMembershipPayment(*terms, payment, params.Rail, params.TransactionID); err != nil {
						return nil, nil, err
					}
				}
				before := terms.PeriodEnd
				if terms.Pending {
					before = terms.PeriodStart
				}
				limit, err := safecast.Convert[int32](len(terms.Entitlements) + 2)
				if err != nil {
					return nil, nil, err
				}
				rows, err := txDB.Gen(ctx).ListInitialMembershipGrants(ctx, gen.ListInitialMembershipGrantsParams{MerchantID: mid.UUID(), SubscriptionID: terms.SubscriptionID, Before: before, RowLimit: limit})
				if err != nil {
					return nil, nil, err
				}
				historyTerms := *terms
				if params.InitialPaymentReversal != "" {
					if sub.Status != models.StatusCancelled || len(rows) != 0 {
						return nil, nil, errors.New("reversed initial payment has active membership or grants")
					}
					historyTerms.Entitlements = map[string]*int{}
				}
				if err := ValidateInitialMembershipHistory(mid.UUID(), historyTerms, rows); err != nil {
					return nil, nil, err
				}
				return sub, nil, nil
			}
			snapshot := sub.EntitlementsSpecSnapshot
			if snapshot == nil {
				snapshot = map[string]*int{}
			}
			if prior.DeletedAt != nil || sub.PriceID != terms.PriceID || sub.ProductID != terms.ProductID || sub.PaymentMethodID == nil || *sub.PaymentMethodID != terms.PaymentMethodID || sub.CurrentPeriodStartsAt != nil || sub.CurrentPeriodEndsAt != nil || !reflect.DeepEqual(snapshot, terms.Entitlements) {
				return nil, nil, errors.New("pending membership contradicts accepted first paid period")
			}
		}
		copy := *params
		copy.Amount, copy.Currency, copy.AmountProvided = terms.Amount, terms.Currency, true
		copy.CurrentPeriodStartsAt, copy.CurrentPeriodEndsAt = &terms.PeriodStart, &terms.PeriodEnd
		params = &copy
	}
	subscription, notifications, err := s.createMembershipCore(ctx, txDB, params)
	if err != nil {
		return nil, nil, err
	}
	return subscription, notifications, nil
}

func (s *SubscriptionLifecycleService) createMembershipCore(ctx context.Context, dbb *db.DB, params *CreateMembershipParams) (*models.Subscription, []*models.NotificationQueue, error) {
	if dbb == nil {
		return nil, nil, errors.New("database handle is required")
	}

	priceService := catalog.NewPriceService(dbb)
	productService := catalog.NewProductService(dbb)
	entitlementService := entitlements.NewEntitlementService(dbb, s.Clock())
	entitlementService.SetClock(s.Clock()) // Propagate clock for testing
	notificationRepo := NewNotificationQueueRepo(dbb)
	subService := NewSubscriptionService(dbb, priceService, productService, nil, s.Clock())

	var price *models.Price
	var err error
	if terms := params.Prepared; terms != nil {
		price = &models.Price{ID: terms.PriceID, ProductID: terms.ProductID, Amount: terms.RecurringAmount, Currency: terms.Currency, AutoRenew: true}
	} else {
		price, err = priceService.GetByID(ctx, params.PriceID)
	}
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
		found, err := subService.subscriptionRepo.GetByPSPSubscriptionIDForUpdate(ctx, string(params.Rail), strings.TrimSpace(*params.RailSubscriptionID))
		if err != nil && !db.IsNotFound(err) {
			return nil, nil, fmt.Errorf("failed to check existing subscription by rail subscription ID: %w", err)
		}
		if err == nil && params.Prepared != nil && found.ID != params.Prepared.SubscriptionID {
			return nil, nil, errors.New("provider schedule belongs to another local membership")
		}

		if err == nil && found.Status == models.StatusPending {
			if found.CustomerID.String() != params.UserID || found.ProductID != price.ProductID {
				return nil, nil, fmt.Errorf("rail subscription belongs to a different pending subscription")
			}
			existingPendingSub = found
		}
	}

	var paymentService *payments.PaymentService
	paymentRecorded := false
	if params.TransactionID != "" && s.PaymentService != nil {
		paymentService = payments.NewPaymentService(dbb, s.Clock())
		existingPayment, err := paymentService.GetByPSPTransactionID(ctx, params.Rail, params.TransactionID)
		if err != nil && !db.IsNotFound(err) {
			return nil, nil, fmt.Errorf("failed to check existing payment: %w", err)
		}
		if err == nil {
			if params.Prepared != nil {
				if err := ValidateInitialMembershipPayment(*params.Prepared, existingPayment, params.Rail, params.TransactionID); err != nil {
					return nil, nil, err
				}
			}
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
			existingSubscription, err := subService.subscriptionRepo.GetByIDForUpdate(ctx, *existingPayment.SubscriptionID)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to load subscription for duplicate payment transaction: %w", err)
			}
			if existingSubscription.CustomerID.String() != params.UserID {
				return nil, nil, fmt.Errorf("payment transaction subscription belongs to a different user")
			}
			switch existingSubscription.Status {
			case models.StatusActive, models.StatusPastDue:
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": existingSubscription.ID,
					"user_id":         params.UserID,
					"payment_id":      existingPayment.ID,
					"transaction_id":  params.TransactionID,
				}).Info("Membership payment already exists; skipping duplicate membership creation")
				return existingSubscription, nil, nil
			case models.StatusPending:
				// Continue below and activate the paid pending subscription.
			default:
				return nil, nil, fmt.Errorf("payment transaction is linked to a subscription with status %q", existingSubscription.Status)
			}
			if existingPendingSub != nil && existingPendingSub.ID != existingSubscription.ID {
				return nil, nil, fmt.Errorf("payment transaction is linked to a different pending subscription")
			}
			if existingSubscription.ProductID != price.ProductID {
				return nil, nil, fmt.Errorf("payment transaction subscription belongs to a different product")
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": existingSubscription.ID,
				"user_id":         params.UserID,
				"payment_id":      existingPayment.ID,
				"transaction_id":  params.TransactionID,
			}).Info("Membership payment already exists for pending subscription; activating it")
			existingPendingSub = existingSubscription
			paymentRecorded = true
		}
	}

	activeSub, err := subService.GetActiveOrPendingByUserIDAndProductID(ctx, params.UserID, price.ProductID)
	if err != nil && !db.IsNotFound(err) {
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
	if params.Prepared != nil {
		now = params.Prepared.AcceptedAt
	}
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
		periodEndsAt = periodStartsAt.Add(time.Duration(*price.AccessDurationHours) * time.Hour)
	default:
		// #651: no provider period and no declared access duration. A cadence
		// is never invented.
		return nil, nil, fmt.Errorf("create membership on price %s: %w", price.ID, &collection.UnknownCycleError{})
	}
	var product *models.Product
	if terms := params.Prepared; terms != nil {
		product = &models.Product{ID: terms.ProductID, DisplayName: terms.ProductName, EntitlementsSpec: models.CloneEntitlementsSpec(terms.Entitlements)}
		err = nil
	} else {
		product, err = productService.GetByID(ctx, price.ProductID)
	}
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

		if terms := params.Prepared; terms != nil {
			subscription.ID, subscription.PspID = terms.SubscriptionID, terms.PSPID
			subscription.CollectionPolicy = terms.CollectionPolicy
			subscription.PaymentMethodID = &terms.PaymentMethodID
			metadata, err := json.Marshal(params.PaymentMetadata)
			if err != nil {
				return nil, nil, fmt.Errorf("accepted membership metadata: %w", err)
			}
			subscription.Metadata = metadata
			if terms.Pending {
				subscription.Status = models.StatusPending
				subscription.StartedAt = terms.AcceptedAt
				subscription.CurrentPeriodStartsAt, subscription.CurrentPeriodEndsAt = nil, nil
			}
		}
		if params.InitialPaymentReversal != "" {
			at := s.now().UTC()
			kind := models.CancelTypeMerchant
			if params.InitialPaymentReversal == "dispute" {
				kind = models.CancelTypeChargeback
			}
			subscription.Status = models.StatusCancelled
			subscription.CancelType = &kind
			subscription.CancelledAt = &at
			subscription.EndedAt = &at
			subscription.CurrentPeriodEndsAt = &at
			if subscription.CurrentPeriodStartsAt != nil && !subscription.CurrentPeriodStartsAt.Before(at) {
				start := at.Add(-time.Microsecond)
				subscription.CurrentPeriodStartsAt = &start
			}
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

	if params.Prepared != nil && params.Prepared.Pending {
		return subscription, nil, nil
	}

	notifications := make([]*models.NotificationQueue, 0, 1)

	if entitlementService != nil && params.InitialPaymentReversal == "" {
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
		if params.Prepared == nil && !periodEndsAt.UTC().After(s.now().UTC()) {
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
		if len(entNames) > 0 {
			if err := pushEngineRenewalGrace(ctx, entitlementService, subscription, entNames, periodStartsAt, periodEndsAt); err != nil {
				return nil, nil, err
			}
		}
	}

	if params.InitialPaymentReversal == "" {
		notification := &models.NotificationQueue{
			ID:         uuidutil.NewV7(),
			CustomerID: subscription.CustomerID,
			EventType:  models.NotificationPremiumStarted,
		}
		if err := notificationRepo.Create(ctx, notification); err != nil {
			if params.Prepared != nil {
				return nil, nil, fmt.Errorf("accepted membership notification: %w", err)
			}
			log.WithContext(ctx).WithError(err).Error("failed to create membership started notification")
		} else {
			notifications = append(notifications, notification)
		}

	}

	// Create Payment record if payment info is provided
	if params.TransactionID != "" && paymentService != nil && !paymentRecorded {
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
			ID:             uuidutil.NewV7(),
			CustomerID:     subscription.CustomerID,
			PriceID:        price.ID,
			SubscriptionID: &subscription.ID,
			Rail:           params.Rail,
			// or#893: the charge belongs to the account that took it, which is
			// the account the subscription itself names.
			PspID:                    pspIDOf(subscription),
			TransactionID:            params.TransactionID,
			Amount:                   amount,
			ListAmount:               price.Amount,
			Currency:                 currency,
			Status:                   payments.PaymentStatusCompletedValue,
			Metadata:                 withPaidPeriod(params.PaymentMetadata, periodStartsAt),
			EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(subscription.EntitlementsSpecSnapshot),
			AttemptKind:              func() *string { k := payments.AttemptInitial; return &k }(),
			MoneyMovement:            models.MoneyMovementRail, // or#827: the signup charge settled at the rail.
			PurchasedAt:              purchasedAt,
			CreatedAt:                now,
		}
		if params.Prepared != nil {
			payment.ID = params.Prepared.PaymentID
			token := charge.TokenTypePSPToken
			if params.PaymentCustodian != "" {
				token = payments.DefaultTokenType(string(params.Rail), params.PaymentCustodian)
			}
			payment.TokenType = &token
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

// RecordConfirmedChargeWithoutRenewal records a provider-confirmed renewal
// charge whose lifecycle must be preserved: a later period is already applied
// or the subscription was terminally cancelled meanwhile. The completed payment
// uses its accepted terms; current price, benefits, access and period stay intact.
// A terminal cancellation alone flags the charge for refund review.
func (s *SubscriptionLifecycleService) RecordConfirmedChargeWithoutRenewal(ctx context.Context, params *RenewMembershipParams) error {
	if params == nil || strings.TrimSpace(params.TransactionID) == "" {
		return errors.New("confirmed charge requires a transaction id")
	}
	if params.Prepared != nil {
		ctx = db.WithPSPID(ctx, params.Prepared.PSPID)
	}
	return s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		db := db.NewWithPgxTx(tx)
		subscription, err := lockedRenewalSubscription(ctx, db, params)
		if err != nil {
			return fmt.Errorf("subscription not found: %w", err)
		}
		priceID := subscription.PriceID
		snapshot := models.CloneEntitlementsSpec(subscription.EntitlementsSpecSnapshot)
		amount, currency := params.Amount, strings.TrimSpace(params.Currency)
		if terms := params.Prepared; terms != nil {
			if err := terms.Validate(); err != nil {
				return err
			}
			if subscription.ID != terms.SubscriptionID || subscription.CustomerID != terms.CustomerID || subscription.PspID != terms.PSPID || !params.AmountProvided || amount != terms.Amount || currency != terms.Currency {
				return errors.New("confirmed charge does not match the accepted subscription")
			}
			priceID, snapshot = terms.PriceID, models.CloneEntitlementsSpec(terms.Entitlements)
		} else {
			price, err := catalog.NewPriceService(db).GetByID(ctx, subscription.PriceID)
			if err != nil {
				return fmt.Errorf("failed to get price: %w", err)
			}
			if amount <= 0 {
				amount = price.Amount
			}
			if currency == "" {
				currency = price.Currency
			}
		}

		var metadata map[string]any
		if params.PreviousPeriodEnd != nil && len(params.PaymentMetadata) > 0 {
			metadata = make(map[string]any, len(params.PaymentMetadata))
			for key, value := range params.PaymentMetadata {
				metadata[key] = value
			}
		}
		_, terminal := TerminalCancelReason(subscription)
		terminal = terminal || (subscription.CollectionPolicy == models.CollectionPolicyEngine && subscription.Status == models.StatusCancelled)
		if terminal {
			if metadata == nil {
				metadata = map[string]any{}
			}
			metadata["refund_review"] = "confirmed charge on a cancelled subscription"
		}
		now := s.now().UTC()
		payment := &models.Payment{
			ID: uuidutil.NewV7(), CustomerID: subscription.CustomerID, PriceID: priceID, SubscriptionID: &subscription.ID,
			Rail: params.Rail, PspID: pspIDOf(subscription), TransactionID: params.TransactionID,
			Amount: amount, ListAmount: amount, Currency: currency, Status: payments.PaymentStatusCompletedValue,
			Metadata:                 metadata,
			EntitlementsSpecSnapshot: snapshot,
			AttemptKind:              func() *string { k := payments.AttemptRenewal; return &k }(),
			MoneyMovement:            models.MoneyMovementRail, PurchasedAt: now, CreatedAt: now,
		}
		if tt := payments.DefaultTokenType(string(params.Rail), renewalPaymentCustodian(params)); tt != "" {
			payment.TokenType = &tt
		}
		created, err := payments.NewPaymentService(db, s.Clock()).CreateIfNotExists(ctx, payment)
		if err != nil {
			return fmt.Errorf("failed to persist confirmed charge: %w", err)
		}
		if !created {
			existing, err := payments.NewPaymentService(db, s.Clock()).GetByPSPTransactionID(ctx, params.Rail, params.TransactionID)
			if err != nil {
				return err
			}
			if existing.SubscriptionID == nil || *existing.SubscriptionID != subscription.ID || existing.CustomerID != subscription.CustomerID || existing.PriceID != priceID {
				return errors.New("confirmed charge replay belongs to different accepted terms")
			}
			return validateCompletedPayment(existing, amount, currency)
		}
		if created && terminal {
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID, "transaction_id": params.TransactionID, "status": subscription.Status,
			}).Error("confirmed rebill charge on a cancelled subscription; payment recorded without reactivation — refund review required")
		}
		return nil
	})
}

// RenewMembership renews an existing subscription and extends the membership.
// It also creates a Payment record for the renewal transaction.
// If a scheduled downgrade exists (ScheduledPriceID), it will be applied on renewal.
func (s *SubscriptionLifecycleService) RenewMembership(ctx context.Context, params *RenewMembershipParams) error {
	notifications := make([]*models.NotificationQueue, 0, 1)
	if params.Prepared != nil {
		ctx = db.WithPSPID(ctx, params.Prepared.PSPID)
	}
	renewalApplied := false

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
		paymentService := payments.NewPaymentService(db, s.Clock())

		// Lock before checking terminal state or preparing a full-row update.
		subscription, err := lockedRenewalSubscription(ctx, db, params)
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

		var price *models.Price
		var newProduct *models.Product
		applyingDowngrade, planChangeApplied := false, false
		preserveLifecycle := false
		var acceptedPayment *models.Payment
		if terms := params.Prepared; terms != nil {
			if err := terms.Validate(); err != nil {
				return err
			}
			if subscription.ID != terms.SubscriptionID || subscription.CustomerID != terms.CustomerID || subscription.PspID != terms.PSPID {
				return errors.New("renewal scope does not match the accepted subscription")
			}
			if !params.AmountProvided || params.Amount != terms.Amount || params.Currency != terms.Currency || params.TransactionID == "" {
				return errors.New("confirmed charge does not match the accepted renewal terms")
			}
			// A webhook or an earlier completion may already have materialized this
			// exact payment. Check before applying a change or advancing the period.
			existing, err := paymentService.GetByPSPTransactionID(ctx, params.Rail, params.TransactionID)
			if err == nil {
				if existing.SubscriptionID == nil || *existing.SubscriptionID != terms.SubscriptionID || existing.CustomerID != terms.CustomerID || existing.PriceID != terms.PriceID {
					return errors.New("renewal transaction already belongs to different accepted terms")
				}
				if err := validateCompletedPayment(existing, terms.Amount, terms.Currency); err != nil {
					return err
				}
				acceptedPayment = existing
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("load accepted renewal payment: %w", err)
			}
			preserveLifecycle, err = applyRenewalTerms(ctx, db, subscription, *terms, params.PreviousPeriodEnd)
			if err != nil {
				return err
			}
			price = &models.Price{ID: terms.PriceID, ProductID: terms.ProductID, Amount: terms.Amount, Currency: terms.Currency}
			newProduct = &models.Product{ID: terms.ProductID, DisplayName: terms.ProductName, EntitlementsSpec: models.CloneEntitlementsSpec(terms.Entitlements)}
			applyingDowngrade = terms.ScheduledPriceID != nil
			planChangeApplied = terms.FromProductID != terms.ProductID
		} else {
			// #773: pick up a due scheduled reprice at the renewal boundary — v1's
			// ONLY effective moment is "the subscription's first renewal on/after
			// effective_at". Re-pin BEFORE the downgrade check below so the normal-
			// renewal price resolution (the else branch) sees the repriced value.
			// Idempotent: a scheduled row that already applied is gone, so a second
			// RenewMembership call for the same renewal (e.g. a caller that also
			// pre-resolves price before charging) just sees no due reprice here.
			// planChangeApplied (#813): a due kind=plan_change reprice moved the
			// subscription across products at this boundary — the downgrade
			// entitlement-diff pass below must run for it too.

			repriceRepo := NewRepriceRepo(db)
			if scheduledReprice, repriceErr := repriceRepo.GetScheduledForSubscription(ctx, subscription.ID); repriceErr == nil {
				if scheduledReprice.IsDue(s.now()) {
					repricedTo, err := priceService.GetByID(ctx, scheduledReprice.ToPriceID)
					if err != nil {
						return fmt.Errorf("failed to get repriced price: %w", err)
					}
					log.WithContext(ctx).WithFields(log.Fields{
						"subscription_id": subscription.ID,
						"reprice_id":      scheduledReprice.ID,
						"kind":            scheduledReprice.Kind,
						"old_price_id":    subscription.PriceID,
						"new_price_id":    repricedTo.ID,
					}).Info("Applying scheduled reprice on renewal")
					if repricedTo.ProductID != subscription.ProductID {
						// #813 plan_change: cross-product cutover — move the
						// product ref and cut entitlement/credit snapshots over at
						// the same boundary the price moves.

						newProduct, err = productService.GetByID(ctx, repricedTo.ProductID)
						if err != nil {
							return fmt.Errorf("failed to get target product for plan change: %w", err)
						}
						subscription.ProductID = repricedTo.ProductID
						subscription.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(newProduct.EntitlementsSpec)
						planChangeApplied = true
					}
					// #773 same-product reprice: price re-pin only.
					subscription.PriceID = repricedTo.ID
					if err := repriceRepo.Apply(ctx, scheduledReprice.ID); err != nil && !errors.Is(err, ErrRepriceNotScheduled) {
						return fmt.Errorf("failed to mark reprice applied: %w", err)
					}
				}
			} else if !errors.Is(repriceErr, pgx.ErrNoRows) {
				return fmt.Errorf("failed to check for scheduled reprice: %w", repriceErr)
			}

			// Check for scheduled downgrade
			applyingDowngrade = subscription.ScheduledPriceID != nil

			if applyingDowngrade {
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
					"old_product_id":  subscription.ProductID,
					"new_product":     newProduct.DisplayName,
				}).Info("Applying scheduled downgrade on renewal")

				// Update subscription to new price and product
				subscription.PriceID = price.ID
				subscription.ProductID = price.ProductID
				subscription.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(newProduct.EntitlementsSpec)
				subscription.ScheduledPriceID = nil // Clear the scheduled downgrade
			} else {
				// Normal renewal - use current price
				price, err = priceService.GetByID(ctx, subscription.PriceID)
				if err != nil {
					return fmt.Errorf("failed to get price: %w", err)
				}
				if len(subscription.EntitlementsSpecSnapshot) == 0 {
					product, err := productService.GetByID(ctx, price.ProductID)
					if err != nil {
						return fmt.Errorf("failed to get product for renewal snapshot: %w", err)
					}
					if len(subscription.EntitlementsSpecSnapshot) == 0 {
						subscription.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(product.EntitlementsSpec)
					}
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

		if params.TransactionID != "" && acceptedPayment == nil {
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
				PspID:                    pspIDOf(subscription),
				TransactionID:            params.TransactionID,
				Amount:                   amount,
				ListAmount:               amount,
				Currency:                 currency,
				Status:                   payments.PaymentStatusCompletedValue,
				Metadata:                 withPaidPeriod(params.PaymentMetadata, renewalPeriodStart(params, subscription, now)),
				EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(subscription.EntitlementsSpecSnapshot),
				AttemptKind:              func() *string { k := payments.AttemptRenewal; return &k }(),
				MoneyMovement:            models.MoneyMovementRail, // or#827: the rebill settled at the rail.
				PurchasedAt:              purchasedAt,
				CreatedAt:                now,
			}
			if tt := payments.DefaultTokenType(string(params.Rail), renewalPaymentCustodian(params)); tt != "" {
				payment.TokenType = &tt
			}
			created, err := paymentService.CreateIfNotExists(ctx, payment)
			if err != nil {
				return fmt.Errorf("failed to persist renewal payment marker: %w", err)
			}
			if !created {
				existingPayment, loadErr := paymentService.GetByPSPTransactionID(ctx, params.Rail, params.TransactionID)
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
				}).Info("Renewal payment already recorded")
				if params.Prepared == nil {
					return nil
				}
			}
		}

		var periodStartsAt, periodEndsAt time.Time
		if terms := params.Prepared; terms != nil {
			periodStartsAt, periodEndsAt = terms.PeriodStart.UTC(), terms.PeriodEnd.UTC()
		} else {
			// The provider's period wins; otherwise the price's cadence extends
			// it. Without either, the renewal fails closed rather than
			// inventing a month.
			switch {
			case params.CurrentPeriodStartsAt != nil && !params.CurrentPeriodStartsAt.IsZero():
				periodStartsAt = params.CurrentPeriodStartsAt.UTC()
			case subscription.CurrentPeriodEndsAt != nil && !subscription.CurrentPeriodEndsAt.IsZero():
				periodStartsAt = *subscription.CurrentPeriodEndsAt
			default:
				periodStartsAt = s.now()
			}
			if params.CurrentPeriodEndsAt != nil && !params.CurrentPeriodEndsAt.IsZero() && params.CurrentPeriodEndsAt.After(periodStartsAt) {
				periodEndsAt = params.CurrentPeriodEndsAt.UTC()
			} else if cycleHours := collection.BillingCycleHoursOf(price); cycleHours > 0 {
				periodEndsAt = periodStartsAt.Add(time.Duration(cycleHours) * time.Hour)
			} else {
				return fmt.Errorf("renew membership %s on price %s: %w", subscription.ID, price.ID, &collection.UnknownCycleError{CycleHours: cycleHours})
			}
		}

		// Both observed and accepted inputs share one local effect writer.
		// A payment row alone does not finish an accepted renewal.
		productName := ""
		if newProduct != nil {
			productName = newProduct.DisplayName
		}
		notification, err := s.applyRenewalEffects(ctx, db, subscription, renewalEffects{
			PeriodStart: periodStartsAt, PeriodEnd: periodEndsAt,
			RevokeRemoved: params.Prepared != nil || applyingDowngrade || planChangeApplied,
			Downgrade:     applyingDowngrade, ProductName: productName,
			PreserveLifecycle: preserveLifecycle,
		})
		if err != nil {
			return err
		}
		renewalApplied = true
		if notification != nil {
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

	s.DispatchNotifications(ctx, notifications)

	return nil
}

// ResumeMembership restores a reversibly-cancelled subscription and its
// standing entitlement projection in one transaction.
func (s *SubscriptionLifecycleService) ResumeMembership(ctx context.Context, params *ResumeMembershipParams) (*models.Subscription, error) {
	if params == nil || params.SubscriptionID == uuid.Nil {
		return nil, fmt.Errorf("resume membership: subscription id is required")
	}

	observed, err := NewSubscriptionRepo(s.DB).GetByID(ctx, params.SubscriptionID)
	if err != nil {
		return nil, fmt.Errorf("resume membership: load subscription: %w", err)
	}
	now := s.now().UTC()
	var resumed *models.Subscription
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := s.DB.NewWithPgxTx(tx)
		if observed.CollectionPolicy == models.CollectionPolicyEngine {
			if _, err := txdb.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: observed.MerchantID, ID: observed.CustomerID}); err != nil {
				return err
			}
		}
		subscription, err := NewSubscriptionRepo(txdb).GetByIDForUpdate(ctx, params.SubscriptionID)
		if err != nil {
			return fmt.Errorf("resume membership: load subscription: %w", err)
		}
		if subscription.CustomerID != observed.CustomerID || subscription.CollectionPolicy != observed.CollectionPolicy {
			return errors.New("resume membership: accepted customer or collection ownership changed")
		}
		if !Resumable(subscription, now) {
			return fmt.Errorf("resume membership: subscription %s is not resumable", subscription.ID)
		}

		if subscription.CollectionPolicy == models.CollectionPolicyEngine {
			q := txdb.Gen(ctx)
			method, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: subscription.MerchantID, ID: *subscription.PaymentMethodID})
			if err != nil {
				return err
			}
			observedInstrument := charge.FreezeInstrument(method)
			if method.CustodianID != nil {
				handle := paymentmethods.CustodianHandle{Custodian: *method.CustodianID, Method: method.RailMethodRef}
				if err := paymentmethods.LockCustodianHandles(ctx, q, subscription.MerchantID, handle); err != nil {
					return err
				}
				if err := paymentmethods.RequireCustodianHandleAvailable(ctx, q, subscription.MerchantID, handle); err != nil {
					return err
				}
			}
			method, err = q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: subscription.MerchantID, ID: *subscription.PaymentMethodID})
			if err != nil {
				return err
			}
			if method.CustomerID != subscription.CustomerID || method.PspID != subscription.PspID || method.ParkReason != "" || method.StoredCredentialRecurringRef == "" || method.Rail != string(subscription.Rail) || method.RailCustomerRef == "" || method.RailMethodRef == "" || (method.Custodian != models.CustodianHyperSwitch && method.Custodian != models.CustodianPSP) || observedInstrument.Matches(method, charge.AgreementRecurring) != nil {
				return fmt.Errorf("resume engine: payment method is unavailable")
			}
		}
		subscription.Status = models.StatusActive
		subscription.CancelledAt = nil
		subscription.CancelType = nil
		subscription.CancelFeedback = nil
		subscription.EndedAt = nil
		if err := NewSubscriptionRepo(txdb).UpdateAt(ctx, subscription, now); err != nil {
			return fmt.Errorf("resume membership: update subscription: %w", err)
		}
		entSvc := s.newLifecycleEntitlementService(txdb)
		if err := entSvc.ResumeSubscriptionAccess(ctx, subscription.ID); err != nil {
			return fmt.Errorf("resume membership: reopen subscription access: %w", err)
		}
		if subscription.CurrentPeriodStartsAt != nil && subscription.CurrentPeriodEndsAt != nil {
			if err := pushEngineRenewalGrace(ctx, entSvc, subscription, entitlementNames(subscription.EntitlementsSpecSnapshot), *subscription.CurrentPeriodStartsAt, *subscription.CurrentPeriodEndsAt); err != nil {
				return fmt.Errorf("resume membership: %w", err)
			}
		}
		resumed = subscription
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resumed, nil
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
		subService := NewSubscriptionService(txdb, priceService, productService, nil, s.Clock())
		entitlementService := entitlements.NewEntitlementService(txdb, s.Clock())
		entitlementService.SetClock(s.Clock())

		subscription, err := subService.subscriptionRepo.GetByPSPSubscriptionIDForUpdate(ctx, string(params.Rail), railSubID)
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

		if len(subscription.EntitlementsSpecSnapshot) == 0 {
			product, err := productService.GetByID(ctx, price.ProductID)
			if err != nil {
				return fmt.Errorf("failed to load subscription product: %w", err)
			}
			if len(subscription.EntitlementsSpecSnapshot) == 0 {
				subscription.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(product.EntitlementsSpec)
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

		// #691 resume: re-open the advance-written cancel closure (end_at back to
		// NULL) so an auto-renew resume restores STANDING access; the pushes below
		// then only record the paid-period fact.
		if err := entitlementService.ResumeSubscriptionAccess(ctx, subscription.ID); err != nil {
			return fmt.Errorf("failed to resume subscription access windows: %w", err)
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

	var result *CancelMembershipTxResult
	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = s.CancelMembershipTx(ctx, db.NewWithPgxTx(tx), params)
		return err
	})
	if err != nil {
		return err
	}

	s.DispatchNotifications(ctx, result.Notifications)
	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id": result.SubscriptionID,
		"user_id":         result.UserID,
	}).Info("Membership cancellation flow completed")

	return nil
}

// CancelMembershipTx applies cancellation using the caller's transaction. The
// caller owns commit/rollback and must dispatch the returned notifications only
// after a successful commit.
func (s *SubscriptionLifecycleService) CancelMembershipTx(ctx context.Context, txDB *db.DB, params *CancelMembershipParams) (*CancelMembershipTxResult, error) {
	if txDB == nil {
		return nil, errors.New("transaction DB is required")
	}

	priceService := catalog.NewPriceService(txDB)
	productService := catalog.NewProductService(txDB)
	notificationRepo := NewNotificationQueueRepo(txDB)
	subService := NewSubscriptionService(txDB, priceService, productService, nil, s.Clock())

	var subscription *models.Subscription
	var err error
	if params.SubscriptionID != nil {
		subscription, err = subService.subscriptionRepo.GetByIDForUpdate(ctx, *params.SubscriptionID)
	} else if params.RailSubscriptionID != nil && params.Rail != nil {
		subscription, err = subService.subscriptionRepo.GetByPSPSubscriptionIDForUpdate(ctx, string(*params.Rail), *params.RailSubscriptionID)
	} else {
		return nil, fmt.Errorf("either subscription_id or rail details must be provided")
	}
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("Failed to locate subscription for cancellation")
		return nil, fmt.Errorf("subscription not found: %w", err)
	}

	result := &CancelMembershipTxResult{
		SubscriptionID: subscription.ID,
		UserID:         subscription.CustomerID.String(),
		Notifications:  make([]*models.NotificationQueue, 0, 1),
	}
	// A late event must preserve the existing cancellation and its terminal reason.
	if subscription.Status == models.StatusCancelled && NormalizeCancelType(subscription.CancelType) == string(models.CancelTypeChargeback) {
		return result, nil
	}

	// A repeated refund/merchant reversal cannot reactivate an already revoked
	// engine agreement or move its terminal dates. No paid/grace interval remains.
	if subscription.CollectionPolicy == models.CollectionPolicyEngine && subscription.Status == models.StatusCancelled && NormalizeCancelType(subscription.CancelType) == string(models.CancelTypeMerchant) && params.CancelType == models.CancelTypeMerchant && subscription.EndedAt != nil && !subscription.EndedAt.After(s.now()) && subscription.CurrentPeriodEndsAt != nil && !subscription.CurrentPeriodEndsAt.After(s.now()) {
		return result, nil
	}

	// Merchant cancellation admits active or collecting engine obligations under
	// the same row lock as the mutation. A later terminal state stays terminal.
	if subscription.CollectionPolicy == models.CollectionPolicyEngine && params.CancelType == models.CancelTypeMerchant && subscription.Status != models.StatusActive && subscription.Status != models.StatusPastDue {
		return nil, ErrSubscriptionNotActive
	}

	// A replayed engine user cancel cannot soften a later merchant or system
	// cancellation. The locked current row is the lifecycle authority.
	if subscription.CollectionPolicy == models.CollectionPolicyEngine && subscription.Status == models.StatusCancelled && params.CancelType == models.CancelTypeUser {
		return result, nil
	}

	// Cancellation policy (caller-owned): an immediate revoke truncates the
	// paid period to now; a period-end cancel keeps paid access until the term
	// ends and only forfeits the pre-appended #368 grace window. The terminal
	// status flip + Solana cascade + entitlement revoke are the shared local-
	// state core (ApplyLocalCancellation), so this path can never diverge from
	// the LIFE-plane convergence repairs.
	now := s.now()
	endAt := now
	if params.RevokeAccess {
		subscription.CurrentPeriodEndsAt = &now
		if subscription.CurrentPeriodStartsAt != nil && !subscription.CurrentPeriodStartsAt.Before(now) {
			adjustedStart := now.Add(-time.Second)
			subscription.CurrentPeriodStartsAt = &adjustedStart
		}
	} else if subscription.CurrentPeriodEndsAt != nil && subscription.CurrentPeriodEndsAt.After(now) {
		endAt = *subscription.CurrentPeriodEndsAt
	}

	immediate := params.RevokeAccess || subscription.CurrentPeriodEndsAt == nil || !subscription.CurrentPeriodEndsAt.After(now)
	revokeReason := models.EntitlementRevokeAdmin
	revokeSources := []models.EntitlementSourceType{models.EntitlementSourceGrace}
	if immediate {
		if params.CancelType == models.CancelTypeChargeback {
			revokeReason = models.EntitlementRevokeChargeback
		}
		revokeSources = []models.EntitlementSourceType{models.EntitlementSourceSubscription, models.EntitlementSourceGrace}
	}

	if err := s.ApplyLocalCancellation(ctx, txDB, subscription, LocalCancellation{
		EndedAt:       endAt,
		CancelType:    params.CancelType,
		Feedback:      params.CancelFeedback,
		RevokeReason:  revokeReason,
		RevokeAsOf:    now,
		RevokeSources: revokeSources,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithField("subscription_id", subscription.ID).Error("Failed to apply local cancellation")
		return nil, err
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
		Data:       openrails.NotificationData{Reason: string(reason)},
	}
	if err := notificationRepo.Create(ctx, notification); err != nil {
		log.WithContext(ctx).WithError(err).Error("failed to create membership ended notification")
	} else {
		result.Notifications = append(result.Notifications, notification)
	}

	return result, nil
}

// CancelMembershipTxResult contains commit-safe cancellation side effects.
type CancelMembershipTxResult struct {
	SubscriptionID uuid.UUID
	UserID         string
	Notifications  []*models.NotificationQueue
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

	if err := NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, now); err != nil {
		return fmt.Errorf("apply local cancellation: update subscription %s: %w", sub.ID, err)
	}

	// #264 cascade: stop the Solana cranker in the same transaction. A genuinely
	// missing mirror remains an idempotent no-op; every other error must roll the
	// parent cancellation back so a retry cannot leave the cranker active.
	if sub.Rail == models.RailSolana {
		if err := s.cancelSolanaSubscriptionForLifecycle(ctx, dbb, sub.ID); err != nil {
			return fmt.Errorf("apply local cancellation: cancel Solana subscription %s: %w", sub.ID, err)
		}
	}

	entSvc := s.newLifecycleEntitlementService(dbb)
	if len(c.RevokeSources) > 0 {
		if err := entSvc.RevokeSourcesForSubscriptionAsOf(ctx, sub.CustomerID.String(), sub.ID, c.RevokeAsOf, c.RevokeReason, c.RevokeSources...); err != nil {
			return fmt.Errorf("apply local cancellation: revoke subscription %s entitlements: %w", sub.ID, err)
		}
	}
	// #691 closure: a terminal cancel is PROOF — write the window end on disk now
	// (period-end cancels leave the paid runway; the standing window must not
	// outlive it). Idempotent; no-op when the revoke above already closed access.
	if err := entSvc.BoundSubscriptionAccess(ctx, sub.ID, endedAt); err != nil {
		return fmt.Errorf("apply local cancellation: bound subscription %s access: %w", sub.ID, err)
	}
	return nil
}

// withLockedSubscription refreshes a caller snapshot under the same row lock
// used by interactive lifecycle operations. Reconciliation/dunning snapshots can
// become stale while provider evidence is fetched, so their guards must inspect
// the locked current row too.
func withLockedSubscription(ctx context.Context, database *db.DB, snapshot *models.Subscription, apply func(context.Context, *db.DB, *models.Subscription) error) error {
	if database == nil || snapshot == nil {
		return errors.New("subscription mutation requires a database and subscription")
	}
	var current *models.Subscription
	err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := database.NewWithPgxTx(tx)
		var err error
		current, err = NewSubscriptionRepo(txdb).GetByIDForUpdate(ctx, snapshot.ID)
		if err != nil {
			return err
		}
		return apply(ctx, txdb, current)
	})
	if err == nil {
		*snapshot = *current
	}
	return err
}

// ApplyLocalPastDue is the side-effect-free LOCAL transition of an active sub
// into dunning (past_due), grace dated to the supplied instant (the missed
// period end). #664: an already-exhausted grace is later parked as `unknown` by
// grace_exhausted, never terminated — FailMembership owns terminal
// cancellation. Grace is set only when none exists. No-op unless active; runs
// under a row lock on the supplied `dbb`; an outer transaction may extend atomicity.
func (s *SubscriptionLifecycleService) ApplyLocalPastDue(ctx context.Context, dbb *db.DB, sub *models.Subscription, graceEndsAt time.Time) error {
	belief := sub.CurrentPeriodEndsAt
	return withLockedSubscription(ctx, dbb, sub, func(ctx context.Context, dbb *db.DB, sub *models.Subscription) error {
		if sub.Status != models.StatusActive || !samePeriodEnd(belief, sub.CurrentPeriodEndsAt) {
			return nil // idempotent: only an active sub on the decided period enters dunning here
		}
		sub.Status = models.StatusPastDue
		if sub.GraceEndsAt == nil {
			ge := graceEndsAt
			sub.GraceEndsAt = &ge
		}
		if err := NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, s.now()); err != nil {
			return fmt.Errorf("apply local past_due: update subscription %s: %w", sub.ID, err)
		}
		return nil
	})
}

// ResumeStalledDunning restores the retry of an OpenRails-dunned NMI schedule
// (provider_dunning) that is past_due with no attempt scheduled: the
// schedule's next step after its last recorded decline, clamped to grace.
// Without a recorded decline, past grace, or with the schedule spent, it does
// nothing. Reports whether a retry was scheduled.
func (s *SubscriptionLifecycleService) ResumeStalledDunning(ctx context.Context, dbb *db.DB, subscriptionID uuid.UUID) (bool, error) {
	resumed := false
	err := withLockedSubscription(ctx, dbb, &models.Subscription{ID: subscriptionID}, func(ctx context.Context, dbb *db.DB, sub *models.Subscription) error {
		now := s.now()
		if sub.CollectionPolicy != models.CollectionPolicyProviderDunning || sub.Status != models.StatusPastDue || sub.NextRetryAt != nil ||
			sub.RetryAttempts == nil || *sub.RetryAttempts < 1 || sub.LastRetryAt == nil {
			return nil
		}
		if sub.GraceEndsAt != nil && !sub.GraceEndsAt.After(now) {
			return nil
		}
		price, err := catalog.NewPriceService(dbb).GetByID(ctx, sub.PriceID)
		if err != nil {
			return fmt.Errorf("resume dunning %s: load price: %w", sub.ID, err)
		}
		next, ok, err := collection.NextAttemptAt(collection.BillingCycleHoursOf(price), *sub.RetryAttempts, *sub.LastRetryAt)
		if err != nil && !errors.Is(err, collection.ErrUnknownCycle) {
			return err
		}
		if !ok {
			return nil // spent, or an unknown cycle the due pass reports
		}
		if sub.GraceEndsAt != nil && next.After(*sub.GraceEndsAt) {
			next = *sub.GraceEndsAt
		}
		sub.NextRetryAt = &next
		if err := NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, now); err != nil {
			return fmt.Errorf("resume dunning %s: %w", sub.ID, err)
		}
		resumed = true
		return nil
	})
	return resumed, err
}

// ApplyLocalUnknown parks a subscription as `unknown` (#632/#664): a
// needs-provider-verification state resolved by provider-pull (#633). Entry
// from `active` (period elapsed, no ownership evidence) or `past_due` (dunning
// stalled past grace). Access stays intact — no revoke on a guess. Clears stale
// grace/retry scheduling; keeps retry_attempts/last_retry_at as attempt
// evidence. from narrows the entry statuses, checked under the row lock.
func (s *SubscriptionLifecycleService) ApplyLocalUnknown(ctx context.Context, dbb *db.DB, sub *models.Subscription, from ...models.SubscriptionStatus) error {
	if len(from) == 0 {
		from = []models.SubscriptionStatus{models.StatusActive, models.StatusPastDue}
	}
	belief := sub.CurrentPeriodEndsAt
	return withLockedSubscription(ctx, dbb, sub, func(ctx context.Context, dbb *db.DB, sub *models.Subscription) error {
		if !slices.Contains(from, sub.Status) || (sub.Status != models.StatusActive && sub.Status != models.StatusPastDue) {
			return nil // idempotent: only active/past_due rows (narrowed by from) enter verification limbo
		}
		if !samePeriodEnd(belief, sub.CurrentPeriodEndsAt) {
			return nil // the caller decided on a period that has since moved (a renewal landed)
		}
		sub.Status = models.StatusUnverified
		sub.GraceEndsAt = nil
		sub.NextRetryAt = nil
		if err := NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, s.now()); err != nil {
			return fmt.Errorf("apply local unknown: update subscription %s: %w", sub.ID, err)
		}
		return nil
	})
}

// samePeriodEnd reports whether the locked row is still on the period a caller
// decided on. A caller with no period belief (nil) accepts any.
func samePeriodEnd(belief, current *time.Time) bool {
	if belief == nil {
		return true
	}
	return current != nil && current.Equal(*belief)
}

// UnknownResolution is the provider-confirmed outcome for an `unknown` subscription,
// produced by the #633 batched provider-pull and applied by ResolveUnknownSubscription.
type UnknownResolution int

const (
	// ResolveUnreachable: the provider was not reachable (no creds / down / rate
	// limited). The sub STAYS unknown and is retried with exponential backoff (#633).
	ResolveUnreachable UnknownResolution = iota
	// ResolveRenewed: the provider confirms a VERIFIED renewal charge for the new
	// period. Advance the local period to the provider's period end and return to
	// `active` (the backfilled payment is the charge that justifies it).
	ResolveRenewed
	// ResolveAdopted (#367 doctrine, ported by #665): the remote sub is alive with
	// a FUTURE next billing but NO verified charge (provider clock misalignment).
	// Re-anchor the period END to the provider's clock and return to `active`;
	// the period START is untouched so no new entitlement window is derived —
	// adoption alone never grants access.
	ResolveAdopted
	// ResolvePastDue: the provider confirms the renewal payment FAILED but the sub is
	// still recoverable within the dunning window. Enter `past_due` so dunning/grace
	// runs (an our-rebill sub) or grace_exhausted terminates it.
	ResolvePastDue
	// ResolveCancelled: the provider deleted/cancelled the remote subscription.
	// Terminal: cancel locally and revoke the access window as-of the period end.
	ResolveCancelled
	// ResolveCancelledRemoteAlive (#679): terminal cancel where the REMOTE
	// subscription may still exist and keep retrying (stale-decline resolution —
	// the roster did not confirm it gone). Same local transition as
	// ResolveCancelled, plus the deferred NMI delete is durably queued
	// (DeletionScheduledAt marker + nmi_delete intent; execution mode-gated).
	ResolveCancelledRemoteAlive
)

// ResolveUnknownSubscription applies a provider-confirmed outcome to an `unknown`
// subscription (#632). Side-effect-free local-state transition on the supplied
// `dbb` (the caller owns atomicity), mirroring ApplyLocalPastDue/Cancellation.
// No-op unless the sub is currently `unknown` (idempotent — a concurrent resolve or
// a re-run of the same pull lands the same state once). newPeriodEnd is the
// provider's confirmed period end (used by ResolveRenewed); graceEndsAt dates the
// dunning grace window (ResolvePastDue), normally the missed period end.
func (s *SubscriptionLifecycleService) ResolveUnknownSubscription(ctx context.Context, dbb *db.DB, sub *models.Subscription, res UnknownResolution, newPeriodStart, newPeriodEnd *time.Time, graceEndsAt time.Time) error {
	return withLockedSubscription(ctx, dbb, sub, func(ctx context.Context, dbb *db.DB, sub *models.Subscription) error {
		if sub.Status != models.StatusUnverified {
			return nil // idempotent
		}
		now := s.now()
		switch res {
		case ResolveUnreachable:
			return nil // stay unknown; #633 retries with backoff
		case ResolveRenewed:
			sub.Status = models.StatusActive
			if newPeriodEnd != nil {
				// New period starts where the provider says it does, else at the
				// prior period end (or now if unknown), and ends at the
				// provider-confirmed end. The renewal payment is backfilled by #634.
				start := now
				if sub.CurrentPeriodEndsAt != nil {
					start = *sub.CurrentPeriodEndsAt
				}
				if newPeriodStart != nil && newPeriodStart.Before(*newPeriodEnd) {
					start = newPeriodStart.UTC()
				}
				if newPeriodEnd.After(start) {
					sub.CurrentPeriodStartsAt = &start
					end := *newPeriodEnd
					sub.CurrentPeriodEndsAt = &end
				}
			}
			sub.ClearRetrySchedule()
			// A provider-billed period-end tier change: the provider already
			// bills the scheduled price, so its renewal opens the new tier.
			scheduled := sub.CollectionPolicy != models.CollectionPolicyEngine && rails.IsNMI(sub.Rail) && sub.ScheduledPriceID != nil && newPeriodEnd != nil && sub.CurrentPeriodStartsAt != nil && sub.CurrentPeriodEndsAt.Equal(*newPeriodEnd)
			if scheduled {
				price, err := catalog.NewPriceService(dbb).GetByID(ctx, *sub.ScheduledPriceID)
				if err != nil {
					return fmt.Errorf("resolve unknown (renewed) %s scheduled price: %w", sub.ID, err)
				}
				product, err := catalog.NewProductService(dbb).GetByID(ctx, price.ProductID)
				if err != nil {
					return fmt.Errorf("resolve unknown (renewed) %s scheduled product: %w", sub.ID, err)
				}
				sub.PriceID, sub.ProductID, sub.ScheduledPriceID = price.ID, product.ID, nil
				sub.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(product.EntitlementsSpec)
			}
			if err := NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, now); err != nil {
				return fmt.Errorf("resolve unknown (renewed) %s: %w", sub.ID, err)
			}
			if scheduled {
				return s.switchTierAccess(ctx, dbb, sub, *sub.CurrentPeriodStartsAt, *sub.CurrentPeriodEndsAt)
			}
			return nil
		case ResolveAdopted:
			// The provider's period: its end, and its start when stated. No
			// entitlement windows are written (adoption alone never grants
			// access; a real charge renews).
			sub.Status = models.StatusActive
			if newPeriodEnd != nil {
				end := *newPeriodEnd
				sub.CurrentPeriodEndsAt = &end
				if newPeriodStart != nil && newPeriodStart.Before(end) {
					start := newPeriodStart.UTC()
					sub.CurrentPeriodStartsAt = &start
				}
			}
			sub.ClearRetrySchedule()
			if err := NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, now); err != nil {
				return fmt.Errorf("resolve unknown (adopted) %s: %w", sub.ID, err)
			}
			return nil
		case ResolvePastDue:
			sub.Status = models.StatusPastDue
			if sub.GraceEndsAt == nil {
				ge := graceEndsAt
				sub.GraceEndsAt = &ge
			}
			if err := NewSubscriptionRepo(dbb).UpdateAt(ctx, sub, now); err != nil {
				return fmt.Errorf("resolve unknown (past_due) %s: %w", sub.ID, err)
			}
			return nil
		case ResolveCancelled, ResolveCancelledRemoteAlive:
			asOf := now
			if sub.CurrentPeriodEndsAt != nil {
				asOf = *sub.CurrentPeriodEndsAt
			}
			fb := "cancelled at provider (converged from unknown)"
			// #679 queue-always: the remote sub may still exist and keep retrying
			// (stale decline, roster didn't confirm gone) — durably record the
			// deferred NMI delete like FailMembership.
			scheduleDelete := false
			// or#842: the automated delete is due after a cooling-off window, not at
			// `now`. The handler's relevance re-check supersedes it if this row stops
			// being a cancelled-awaiting-delete one in the meantime, so a convergence
			// we got wrong never reaches the provider.
			deleteAt := SystemDeferredDeleteAt(sub, now)
			if res == ResolveCancelledRemoteAlive {
				fb = "renewal declined beyond dunning window (converged from unknown)"
				if rails.RemoteDeleteOnTerminalCancel(sub.Rail) && sub.RailSubscriptionID != "" {
					if s.deferDelete != nil {
						sub.DeletionScheduledAt = &deleteAt
						scheduleDelete = true
					} else {
						log.WithContext(ctx).WithFields(log.Fields{
							"subscription_id":      sub.ID,
							"rail":                 sub.Rail,
							"rail_subscription_id": sub.RailSubscriptionID,
						}).Warn("no deferred-delete scheduler wired: nmi_delete intent NOT queued; remote rail subscription may still be retrying (wiring gap)")
					}
				}
			}
			lcArgs := LocalCancellation{
				EndedAt:       now,
				CancelType:    models.CancelTypeExpired,
				Feedback:      &fb,
				RevokeReason:  models.EntitlementRevokeDunning,
				RevokeAsOf:    asOf,
				RevokeSources: []models.EntitlementSourceType{models.EntitlementSourceSubscription, models.EntitlementSourceGrace},
			}
			if !scheduleDelete {
				return s.ApplyLocalCancellation(ctx, dbb, sub, lcArgs)
			}
			// Marker + intent commit atomically, same invariant as FailMembership:
			// no crash window between the cancellation UPDATE and the enqueue.
			return dbb.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				txdb := db.NewWithPgxTx(tx)
				if err := s.ApplyLocalCancellation(ctx, txdb, sub, lcArgs); err != nil {
					return err
				}
				return s.deferDelete.WithTx(tx).ScheduleNMIDelete(ctx, sub.CustomerID.String(), sub.ID, deleteAt)
			})
		default:
			return fmt.Errorf("resolve unknown: unknown resolution %d", res)
		}

	})
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
	solanaRepo := solanasubs.NewSolanaSubscriptionRepo(d)
	row, err := solanaRepo.GetBySubscriptionID(ctx, subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
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
		notificationRepo := NewNotificationQueueRepo(db)
		subService := NewSubscriptionService(db, priceService, productService, nil, s.Clock())
		entSvc := s.newLifecycleEntitlementService(db)

		subscription, err := subService.subscriptionRepo.GetByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			log.WithContext(ctx).WithError(err).Warn("Failed to locate subscription for expiration")
			return fmt.Errorf("subscription not found: %w", err)
		}

		// A late event must preserve the existing cancellation and its terminal reason.
		if subscription.Status == models.StatusCancelled {
			return nil
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
				return fmt.Errorf("list entitlements for expired subscription %s: %w", subscription.ID, err)
			}
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
					return fmt.Errorf("revoke entitlement %q for expired subscription %s: %w", entName, subscription.ID, err)
				}
			}

			// Terminal expiration: immediately remove any grace windows for this subscription too.
			graceNames, err := entSvc.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceGrace, subscription.ID)
			if err != nil {
				return fmt.Errorf("list grace entitlements for expired subscription %s: %w", subscription.ID, err)
			}
			st = models.EntitlementSourceGrace
			for _, entName := range graceNames {
				if err := entSvc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
					UserID:      subscription.CustomerID.String(),
					Entitlement: entName,
					SourceType:  &st,
					SourceID:    &sid,
					Reason:      models.EntitlementRevokeDunning,
				}); err != nil {
					return fmt.Errorf("revoke grace entitlement %q for expired subscription %s: %w", entName, subscription.ID, err)
				}
			}
		}

		notification := &models.NotificationQueue{
			ID:         uuidutil.NewV7(),
			CustomerID: subscription.CustomerID,
			EventType:  models.NotificationPremiumEnded,
			Data:       openrails.NotificationData{Reason: string(PremiumEndReasonExpired)},
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

	s.DispatchNotifications(ctx, notifications)
	log.WithContext(ctx).WithField("subscription_id", subscriptionID).Info("Membership expiration flow completed")

	return nil
}

// recordFailedRenewalAttempt writes the declined renewal charge as a durable
// status='failed' payments row (#733) inside the caller's tx. Best-effort:
// a missing price is logged, never fails the dunning flow. Idempotent on the
// synthetic per-attempt transaction id.
func (s *SubscriptionLifecycleService) recordFailedRenewalAttempt(ctx context.Context, txDB *db.DB, priceService *catalog.PriceService, subscription *models.Subscription, params *FailMembershipParams, now time.Time, attemptNum int) {
	price := subscription.Price
	if price == nil && subscription.PriceID != uuid.Nil {
		if p, err := priceService.GetByID(ctx, subscription.PriceID); err == nil {
			price = p
		}
	}
	if price == nil {
		log.WithContext(ctx).WithField("subscription_id", subscription.ID).Warn("declined renewal not recorded as payment row: no price")
		return
	}
	kind := payments.AttemptRenewal
	failed := &models.Payment{
		ID:             uuidutil.NewV7(),
		CustomerID:     subscription.CustomerID,
		PriceID:        price.ID,
		SubscriptionID: &subscription.ID,
		Rail:           subscription.Rail,
		PspID:          pspIDOf(subscription),
		TransactionID:  fmt.Sprintf("renewal_declined:%s:attempt%d", subscription.ID, attemptNum),
		Amount:         price.Amount,
		ListAmount:     price.Amount,
		Currency:       price.Currency,
		Status:         payments.PaymentStatusFailedValue,
		AttemptKind:    &kind,
		MoneyMovement:  models.MoneyMovementNone, // or#827: a decline moved nothing.
		PurchasedAt:    now,
		CreatedAt:      now,
	}
	if code := normalize.FromPtr(params.FailureCode); code != "" {
		reason := payments.NormalizeFailureReason(string(subscription.Rail), code)
		failed.FailureCode = &code
		failed.FailureReason = &reason
	}
	if tt := payments.DefaultTokenType(string(subscription.Rail), models.CustodianPSP); tt != "" {
		failed.TokenType = &tt
	}
	if _, err := payments.NewPaymentService(txDB, s.Clock()).CreateIfNotExists(ctx, failed); err != nil {
		log.WithContext(ctx).WithError(err).WithField("subscription_id", subscription.ID).Error("failed to record declined renewal payment row")
	}
}

// FailMembership marks a subscription as failed due to payment issues.
// FindingTerminalHeld is a terminal decline whose cancellation was refused.
const FindingTerminalHeld = "life.terminal_outcome.held"

func (s *SubscriptionLifecycleService) FailMembership(ctx context.Context, params *FailMembershipParams) error {
	if params == nil || params.SubscriptionID == nil || *params.SubscriptionID == uuid.Nil {
		return fmt.Errorf("subscription_id is required")
	}

	notifications := make([]*models.NotificationQueue, 0, 1)

	// Variables to capture from transaction for the deferred-delete log
	var subscriptionID uuid.UUID
	var userID string
	// Set inside the tx when a terminal cancellation must also stop the
	// remote NMI recurring subscription; the job is enqueued after commit.
	var scheduleDeferredDelete bool
	// or#870 bucket 2: the decline means the customer must fix their card.
	// Drives the payment_method_update_required notification below.
	var needsPaymentMethodUpdate bool
	var unknownCycle uuid.UUID

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
		notificationRepo := NewNotificationQueueRepo(db)
		subService := NewSubscriptionService(db, priceService, productService, nil, s.Clock())
		entSvc := s.newLifecycleEntitlementService(db)

		subscription, err := subService.subscriptionRepo.GetByIDForUpdate(ctx, *params.SubscriptionID)
		if err != nil {
			log.WithContext(ctx).WithError(err).Warn("Failed to locate subscription for failure flow")
			return fmt.Errorf("subscription not found: %w", err)
		}

		// A late event must preserve the existing cancellation and its terminal reason.
		if subscription.Status == models.StatusCancelled {
			return nil
		}

		// Capture values for event logging
		subscriptionID = subscription.ID
		userID = subscription.CustomerID.String()
		scheduleDeferredDelete = false // reset in case the tx is retried
		needsPaymentMethodUpdate = false

		now := s.now()

		// Captured BEFORE either branch mutates subscription.RetryAttempts: the
		// terminal branch below (either path) calls ClearRetrySchedule(), which
		// zeroes RetryAttempts. recordFailedRenewalAttempt's idempotency key is
		// keyed on the attempt ordinal, so reading it AFTER the clear would
		// collapse every terminal attempt's key onto attempt 1 — silently
		// dropping the terminal (most forensically important) decline's payment
		// row as a false CreateIfNotExists replay of attempt 1's row.
		failureAttemptNum := 1
		if subscription.RetryAttempts != nil {
			failureAttemptNum = *subscription.RetryAttempts + 1
		}

		// #821/#839/#840/#836: ONE gate for every terminal outcome in this flow.
		// A terminal cancel revokes entitlements AND queues the IRREVERSIBLE
		// cancellation of the recurring SCHEDULE at the rail (or#870: never the
		// customer's stored payment method — nothing here can delete that), so it
		// requires (a) a named certainty leg —
		// provider truth, a non-retryable decline, or genuinely exhausted dunning
		// ATTEMPTS — and (b) an open operator kill switch. A date comparison, an
		// expired dunning window, and the absence of one of our own rows are not
		// evidence. Refused terminals PARK as `unknown`: access intact, out of the
		// dunning queue, resolved by the provider-verification plane.
		terminalRefusal := func(leg string) string {
			if params.TerminalBlocked != "" {
				return params.TerminalBlocked
			}
			if leg == "" {
				return "no certainty leg named (collection.Certainty*): terminal cancellation requires provider truth, a non-retryable decline, or exhausted dunning attempts"
			}
			return ""
		}
		// scheduleExhaustionLeg: running the retry schedule out is certainty ONLY
		// when the attempts were REAL — a recorded charge attempt per failure
		// (#733 payments row). Attempts we declined to make because our own data
		// was missing (#840) carry no leg and cannot exhaust anything.
		scheduleExhaustionLeg := func() string {
			if params.TerminalCertainty != "" {
				return params.TerminalCertainty
			}
			if params.RecordFailedAttempt || params.AttemptRecorded {
				return collection.CertaintyDunningExhausted
			}
			return ""
		}
		// parkUnknown is ApplyLocalUnknown's shape applied inside this tx: the
		// row leaves the dunning queue without losing the customer's access.
		// awaitingStatus is where a stopped subscription waits. An engine
		// obligation is ours to decide: it waits past_due for the customer's
		// new card (or the operator), never in the provider-verification
		// cohort, which has nothing to probe for it.
		awaitingStatus := models.StatusUnverified
		if subscription.CollectionPolicy == models.CollectionPolicyEngine {
			awaitingStatus = models.StatusPastDue
		}
		heldTerminal := ""
		parkUnknown := func(why string) {
			heldTerminal = why
			subscription.Status = awaitingStatus
			subscription.GraceEndsAt = nil
			subscription.NextRetryAt = nil
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id":    subscription.ID,
				"failure_reason":     normalize.FromPtr(params.FailureReason),
				"terminal_certainty": params.TerminalCertainty,
				"refusal":            why,
			}).Warn("Terminal cancellation REFUSED; parking subscription as unknown (entitlements intact, no provider delete queued)")
		}

		// or#870 bucket 2 — THEIR card, fixable (expired, bad CVC, do-not-honor,
		// call issuer...). Retrying cannot succeed and burns attempts against the
		// issuer, but the customer fixes it in a minute. So: stop charging NOW,
		// keep the subscription alive and its entitlements intact
		// (awaiting_method projects standing access), and notify them to update
		// the payment method; a replaced method resumes dunning. NOT a terminal outcome — no cancel, no revoke, no
		// certainty leg required, and emphatically no touching of their stored
		// card. This is where recoverable revenue lives.
		if params.Decline == collection.DeclineFixPaymentMethod {
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.CustomerID,
				"failure_code":    normalize.FromPtr(params.FailureCode),
			}).Warn("or#870 bucket 2: payment method needs the customer's attention; charging STOPS, access and the stored card are untouched")
			subscription.Status = models.StatusAwaitingMethod
			subscription.GraceEndsAt = nil
			subscription.NextRetryAt = nil
			needsPaymentMethodUpdate = true
		} else if params.Decline == collection.DeclineNonRecoverable && terminalRefusal(params.TerminalCertainty) != "" {
			// Bucket 3 with the #836 kill switch closed (or no certainty leg
			// named): the mandate is gone, so continuing to charge is wrong, but
			// cancelling is forbidden. Park as `unknown` — access intact, out of
			// the dunning queue, no rail action queued.
			parkUnknown(terminalRefusal(params.TerminalCertainty))
		} else if params.Decline == collection.DeclineNonRecoverable {
			// or#870 bucket 3 — the issuer withdrew the recurring mandate, or the
			// instrument is permanently dead. Cancel now with no grace and no
			// further retries; the deferred rail-side SCHEDULE delete below stops
			// NMI rebilling forever. The stored payment method is not touched.
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
				"user_id":         subscription.CustomerID,
				"failure_code":    normalize.FromPtr(params.FailureCode),
			}).Warn("or#870 bucket 3: non-recoverable decline; cancelling the subscription at the rail (stored payment method left intact)")
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
			// first failure is terminal). See collection.RetryOffsets.
			cycleHours := 0
			if subscription.CollectionPolicy == models.CollectionPolicyEngine {
				accepted := params.Prepared
				if accepted == nil {
					return errors.New("engine failure requires its accepted renewal agreement")
				}
				if err := accepted.Validate(); err != nil {
					return err
				}
				cycle := accepted.PeriodEnd.Sub(accepted.PeriodStart)
				if accepted.SubscriptionID != subscription.ID || accepted.CustomerID != subscription.CustomerID || accepted.PSPID != subscription.PspID || accepted.FromPriceID != subscription.PriceID || accepted.FromProductID != subscription.ProductID || cycle%time.Hour != 0 || !accepted.PeriodStart.Add(cycle).Equal(accepted.PeriodEnd) {
					return errors.New("engine failure cadence contradicts its accepted agreement")
				}
				cycleHours = int(cycle / time.Hour)
			} else {
				cycleHours = collection.BillingCycleHoursOf(subscription.Price)
			}
			if cycleHours <= 0 {
				if price, perr := priceService.GetByID(ctx, subscription.PriceID); perr == nil {
					cycleHours = collection.BillingCycleHoursOf(price)
				}
			}
			// An unknown cycle fails closed: nothing is retried or cancelled on
			// a guessed cadence; the caller raises the operator finding.
			maxFailures, err := collection.MaxFailures(cycleHours)
			if err != nil {
				unknownCycle = subscription.MerchantID
				return fmt.Errorf("fail membership %s: %w", subscription.ID, err)
			}

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

			// Terminal (the schedule's max failures reached, or a caller-declared
			// terminal): cancel — but only through the certainty + kill-switch
			// gate. Refused ⇒ park as `unknown`. Otherwise schedule the next retry
			// at the schedule's gap for this failure count (relative to now, so a
			// late worker run never schedules into the past).
			leg := params.TerminalCertainty
			if !params.Terminal {
				leg = scheduleExhaustionLeg()
			}
			if refusal := terminalRefusal(leg); terminal && refusal != "" {
				parkUnknown(refusal)
			} else if terminal {
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
				nextRetry, _, err := collection.NextAttemptAt(cycleHours, *subscription.RetryAttempts, now)
				if err != nil {
					return err
				}
				subscription.NextRetryAt = &nextRetry
			}
		}

		// (#691: no grace windows are appended while dunning runs past the paid
		// term — the auto-renew sub's STANDING window keeps access intact until a
		// terminal outcome closes it.)

		// #733: durably record the declined attempt as a failed payments row in
		// this same tx. Terminal-without-charge callers pass
		// RecordFailedAttempt=false (no attempt happened).
		if params.RecordFailedAttempt {
			s.recordFailedRenewalAttempt(ctx, db, priceService, subscription, params, now, failureAttemptNum)
		}

		// #344 follow-up: a terminal payment-failure cancellation of an
		// NMI-backed subscription must also stop the rail-side recurring
		// subscription, or NMI keeps retrying it monthly forever. The
		// DeletionScheduledAt marker AND the nmi_delete intent are both
		// written inside this transaction (atomic — no crash window between
		// marker and intent). #679 queue-always: the desired provider action is
		// recorded UNCONDITIONALLY — provider_write_mode / credentials / the
		// volume breaker gate EXECUTION at the intent executor, never queuing
		// (limited mode parks system-origin intents until mode=full).
		// or#842: due after a cooling-off window, not at `now` — dunning
		// exhaustion is our own inference, and the handler's relevance re-check
		// supersedes the delete if the row recovers inside the window.
		deferredDeleteAt := SystemDeferredDeleteAt(subscription, now)
		if subscription.Status == models.StatusCancelled &&
			rails.RemoteDeleteOnTerminalCancel(subscription.Rail) &&
			subscription.RailSubscriptionID != "" {
			if s.deferDelete != nil {
				subscription.DeletionScheduledAt = &deferredDeleteAt
				scheduleDeferredDelete = true
			} else {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id":      subscription.ID,
					"rail":                 subscription.Rail,
					"rail_subscription_id": subscription.RailSubscriptionID,
				}).Warn("no deferred-delete scheduler wired: nmi_delete intent NOT queued; remote rail subscription left alive (wiring gap)")
			}
		}

		if err := subService.Update(ctx, subscription); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id": subscription.ID,
			}).Error("Failed to update subscription during failure flow")
			return fmt.Errorf("failed to update subscription: %w", err)
		}
		// A terminal outcome the operator's switch (or a missing certainty
		// leg) refused is a standing finding: the member is neither charged
		// nor cancelled until someone decides.
		if heldTerminal != "" {
			evidence, _ := json.Marshal(map[string]any{"subscription_id": subscription.ID, "collection_policy": subscription.CollectionPolicy, "status": subscription.Status, "refusal": heldTerminal, "failure_code": normalize.FromPtr(params.FailureCode)})
			action := fmt.Sprintf("subscription %s reached a terminal decline but cancellation was refused (%s); it waits %s with no further charges. Arm the destructive-action switch to let terminal outcomes apply, or resolve it by hand", subscription.ID, heldTerminal, subscription.Status)
			if _, err := db.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: subscription.MerchantID, FindingType: FindingTerminalHeld, SubjectKey: subscription.ID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence}); err != nil {
				return fmt.Errorf("record held terminal outcome for %s: %w", subscription.ID, err)
			}
		}
		// Engine access is paid time plus the renewal allowance; a decided
		// decline ends the allowance now.
		if subscription.CollectionPolicy == models.CollectionPolicyEngine && subscription.Status != models.StatusCancelled && entSvc != nil {
			if err := entSvc.RevokeSourcesForSubscriptionAsOf(ctx, subscription.CustomerID.String(), subscription.ID, now, models.EntitlementRevokeDunning, models.EntitlementSourceGrace); err != nil {
				return fmt.Errorf("end engine renewal grace for %s: %w", subscription.ID, err)
			}
		}

		// Terminal cancellation with a remote NMI schedule: enqueue the
		// deferred delete intent IN THIS TRANSACTION so the
		// DeletionScheduledAt marker and the intent commit atomically (no
		// crash window between them).
		if scheduleDeferredDelete {
			if err := s.deferDelete.WithTx(tx).ScheduleNMIDelete(ctx, subscription.CustomerID.String(), subscription.ID, deferredDeleteAt); err != nil {
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

		// Revoke entitlements if subscription is cancelled after max retries or a
		// terminal decline.
		if subscription.Status == models.StatusCancelled && entSvc != nil {
			names, err := entSvc.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceSubscription, subscription.ID)
			if err != nil {
				return fmt.Errorf("list entitlements for failed subscription %s: %w", subscription.ID, err)
			}
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
					return fmt.Errorf("revoke entitlement %q for failed subscription %s: %w", entName, subscription.ID, err)
				}
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": subscription.ID,
			}).Warn("Revoked entitlements after max dunning failures")

			// Terminal dunning failure: remove any grace windows too so access doesn't continue.
			graceNames, err := entSvc.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceGrace, subscription.ID)
			if err != nil {
				return fmt.Errorf("list grace entitlements for failed subscription %s: %w", subscription.ID, err)
			}
			st = models.EntitlementSourceGrace
			for _, entName := range graceNames {
				if err := entSvc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
					UserID:      subscription.CustomerID.String(),
					Entitlement: entName,
					SourceType:  &st,
					SourceID:    &sid,
					Reason:      models.EntitlementRevokeDunning,
				}); err != nil {
					return fmt.Errorf("revoke grace entitlement %q for failed subscription %s: %w", entName, subscription.ID, err)
				}
			}
		}

		// Immediate notification for each outcome, so a customer is
		// never silent-treated through a whole dunning cycle and then suddenly
		// cancelled:
		//   bucket 1, still trying  -> payment_method_failed ("we'll keep trying")
		//   bucket 1, schedule out  -> premium_ended / expired ("we gave up")
		//   bucket 2                -> payment_method_update_required ("fix it,
		//                              your access is still on")
		//   bucket 3                -> premium_ended / non_recoverable ("the
		//                              mandate is gone; re-subscribe")
		eventType := models.NotificationPaymentMethodFailed
		var data openrails.NotificationData
		switch {
		case needsPaymentMethodUpdate:
			eventType = models.NotificationPaymentMethodUpdateRequired
			data.FailureCode = normalize.FromPtr(params.FailureCode)
		case subscription.Status == models.StatusCancelled:
			eventType = models.NotificationPremiumEnded
			endReason := PremiumEndReasonExpired
			if params.Decline == collection.DeclineNonRecoverable {
				endReason = PremiumEndReasonNonRecoverable
			}
			data.Reason = string(endReason)
		}

		notification := &models.NotificationQueue{
			ID:         uuidutil.NewV7(),
			CustomerID: subscription.CustomerID,
			EventType:  eventType,
			Data:       data,
		}
		if err := notificationRepo.Create(ctx, notification); err != nil {
			return fmt.Errorf("create payment failed notification: %w", err)
		} else {
			notifications = append(notifications, notification)
		}

		return nil
	})

	if err != nil {
		if errors.Is(err, collection.ErrUnknownCycle) && unknownCycle != uuid.Nil {
			if ferr := collection.RecordUnknownCycle(ctx, s.DB.Gen(ctx), unknownCycle, params.SubscriptionID.String(), "subscription", err); ferr != nil {
				return errors.Join(err, ferr)
			}
		}
		return err
	}

	// The deferred NMI delete intent committed inside the failure-flow
	// transaction above, atomically with the cancellation + marker. Runs at
	// "now": the undo-window semantics of user cancellations do not apply to
	// dunning exhaustion. Idempotent via the intent ledger's idempotency_key
	// (#358).
	if scheduleDeferredDelete {
		// Enqueued inside the failure-flow transaction above; this is just
		// the operator-visible confirmation.
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscriptionID,
			"user_id":         userID,
		}).Info("scheduled deferred NMI delete after terminal payment failure (committed with the cancellation)")
	}

	s.DispatchNotifications(ctx, notifications)

	return nil
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

// pspIDOf is the subscription's PSP as a payment stamp. or#893: a charge — a
// signup, a rebill, or a decline marker — belongs to the account that attempted
// it, and the subscription row is the authority on which one that is.
func pspIDOf(subscription *models.Subscription) *uuid.UUID {
	if subscription == nil || subscription.PspID == uuid.Nil {
		return nil
	}
	id := subscription.PspID
	return &id
}
