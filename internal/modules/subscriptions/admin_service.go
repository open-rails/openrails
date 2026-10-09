package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/pagination"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
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
	Dunning  *billing.SubscriptionDunning
}

// ListSubscriptions is one page of the merchant's subscriptions matching f,
// newest first, with their prices and products.
func (s *AdminSubscriptionService) ListSubscriptions(ctx context.Context, f GetSubscriptionsFilters, page billing.PageRequest) (billing.ListPage[*AdminSubscriptionResponse], error) {
	subs, err := s.SubscriptionService.ListSubscribers(ctx, f, page)
	if err != nil {
		return billing.ListPage[*AdminSubscriptionResponse]{}, err
	}
	ids := make([]uuid.UUID, 0, len(subs.Items))
	for _, sub := range subs.Items {
		ids = append(ids, sub.PriceID)
	}
	prices, err := s.PriceService.GetWithProductByIDs(ctx, ids)
	if err != nil {
		return billing.ListPage[*AdminSubscriptionResponse]{}, fmt.Errorf("failed to load subscription prices: %w", err)
	}
	if err := LoadScheduledChanges(ctx, s.SubscriptionService.Database(), subs.Items); err != nil {
		return billing.ListPage[*AdminSubscriptionResponse]{}, err
	}
	dunning, err := DunningViews(ctx, s.SubscriptionService.Database(), subs.Items, prices)
	if err != nil {
		return billing.ListPage[*AdminSubscriptionResponse]{}, fmt.Errorf("failed to load subscription dunning: %w", err)
	}
	return pagination.Map(subs, func(sub *models.Subscription) *AdminSubscriptionResponse {
		out := &AdminSubscriptionResponse{Subscription: sub, Dunning: dunning[sub.ID]}
		if price := prices[sub.PriceID]; price != nil {
			out.Price, out.Product = price, price.Product
		}
		return out
	}), nil
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

	if err := LoadScheduledChanges(ctx, s.SubscriptionService.Database(), []*models.Subscription{subscription}); err != nil {
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

	dunning, err := DunningViews(ctx, s.SubscriptionService.Database(), []*models.Subscription{subscription}, map[uuid.UUID]*models.Price{subscription.PriceID: response.Price})
	if err != nil {
		return nil, fmt.Errorf("load subscription dunning: %w", err)
	}
	response.Dunning = dunning[subscription.ID]

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
// canceled while NMI happily kept rebilling. Now the local cancellation and
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

	if subscription.CollectionPolicy == models.CollectionPolicyEngine || subscription.Status == models.StatusCanceled && revokeAccess {
		lifecycle := NewSubscriptionLifecycleService(s.SubscriptionService.Database(), nil, nil, s.EntitlementService, s.NotificationService, nil, s.Clock())
		return lifecycle.CancelMembership(ctx, &CancelMembershipParams{SubscriptionID: &subscription.ID, CancelType: models.CancelTypeMerchant, CancelFeedback: &reason, RevokeAccess: revokeAccess, RefuseOwnedRenewal: true})
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
		return ErrCustomerActionRequired
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
			if err := entSvc.RevokeSourcesForSubscription(ctx, subscription.CustomerID.String(), subscription.ID, models.AccessRevokeAdmin, models.AccessSourceSubscription, models.AccessSourceGrace); err != nil {
				return fmt.Errorf("revoke access: %w", err)
			}
		} else {
			if err := entSvc.RevokeSourcesForSubscription(ctx, subscription.CustomerID.String(), subscription.ID, models.AccessRevokeAdmin, models.AccessSourceGrace); err != nil {
				return fmt.Errorf("end renewal grace: %w", err)
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
		Data:       billing.NotificationData{Reason: string(PremiumEndReasonAdmin)},
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

// SendManualNotification sends a manual notification (admin)
func (s *AdminSubscriptionService) SendManualNotification(ctx context.Context, userID string, eventType models.NotificationEventType, message string) error {
	notification := &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: identity.CustomerIDFromString(userID).UUID(),
		EventType:  eventType,
		Data:       billing.NotificationData{Message: message, Source: "admin_manual"},
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
