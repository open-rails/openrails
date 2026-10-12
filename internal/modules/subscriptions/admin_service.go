package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/destructive"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Additional sentinel errors for admin operations
var (
	ErrUserNotFound      = errors.New("user not found")
	ErrRoleNotFound      = errors.New("role not found")
	ErrRoleGrantNotFound = errors.New("role grant not found")
)

// AdminSubscriptionService handles administrative subscription operations
type AdminSubscriptionService struct {
	destructiveAdmissions *destructive.Admissions
	SubscriptionService   *SubscriptionService
	ProductService        *catalog.ProductService
	PriceService          *catalog.PriceService
	EntitlementService    *entitlements.EntitlementService
	NotificationService   *NotificationService
	PaymentService        *payments.PaymentService
	StripeService         *StripeService
	clock                 clockwork.Clock

	// providerCancel queues the durable, admin-origin provider cancel of a
	// merchant-initiated cancel.
	providerCancel ProviderCancelScheduler
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

// now returns the service clock's time, or time.Now() without one.
func (s *AdminSubscriptionService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// AdminSubscriptionResponse represents a subscription with enriched admin data
type AdminSubscriptionResponse struct {
	*models.Subscription

	Price   *models.Price `json:"price,omitempty"`
	Dunning *billing.SubscriptionDunning
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
	response := &AdminSubscriptionResponse{Subscription: subscription}

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

// rebillInFlight reports whether an OpenRails rebill of sub is still unresolved.
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

// CancelSubscription cancels a subscription for the merchant. The local
// cancellation and the provider-cancel intent commit in one transaction, so the
// row is never terminal while the rail-side schedule survives unconfirmed.
func (s *AdminSubscriptionService) CancelSubscription(ctx context.Context, subscriptionID uuid.UUID, reason string, revokeAccess, accountDeletion bool) error {
	subscription, err := s.requireSubscription(ctx, subscriptionID)
	if err != nil {
		return err
	}

	if subscription.Status == models.StatusCanceled && !revokeAccess {
		return ErrSubscriptionNotActive
	}
	if subscription.Status == models.StatusCanceled && subscription.CurrentPeriodEndsAt != nil && !subscription.CurrentPeriodEndsAt.After(s.now()) {
		return nil
	}
	if subscription.CollectionPolicy == models.CollectionPolicyEngine || subscription.Status == models.StatusCanceled && revokeAccess {
		command, err := CancellationCommand(ctx, subscription, reason, revokeAccess, accountDeletion)
		if err != nil {
			return err
		}
		return destructive.RunLocal(ctx, s.SubscriptionService.Database(), s.destructiveAdmissions, command, func(ctx context.Context, _ pgx.Tx, txDB *db.DB) (bool, error) {
			current, err := s.requireLockedSubscription(ctx, txDB, subscriptionID)
			if err != nil {
				return false, err
			}
			if current.LifecycleRev != subscription.LifecycleRev {
				return false, apperr.Conflictf("subscription changed before cancellation; retry")
			}
			lifecycle := NewSubscriptionLifecycleService(txDB, nil, nil, nil, nil, nil, s.Clock())
			_, err = lifecycle.CancelMembershipTx(ctx, txDB, &CancelMembershipParams{SubscriptionID: &subscription.ID, CancelType: models.CancelTypeMerchant, CancelFeedback: &reason, RevokeAccess: revokeAccess, RefuseOwnedRenewal: true})
			if err != nil {
				return false, err
			}
			after, err := s.requireLockedSubscription(ctx, txDB, subscriptionID)
			return err == nil && after.LifecycleRev != current.LifecycleRev, err
		})
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

	command, err := CancellationCommand(ctx, subscription, reason, revokeAccess, accountDeletion)
	if err != nil {
		return err
	}
	observedRevision := subscription.LifecycleRev
	if err := destructive.RunLocal(ctx, s.SubscriptionService.Database(), s.destructiveAdmissions, command, func(ctx context.Context, tx pgx.Tx, txdb *db.DB) (bool, error) {
		changed := false
		err := func() error {
			var err error
			subscription, err = s.requireLockedSubscription(ctx, txdb, subscriptionID)
			if err != nil {
				return err
			}
			if subscription.Status == models.StatusCanceled {
				return nil
			}
			if subscription.LifecycleRev != observedRevision {
				return apperr.Conflictf("subscription changed before cancellation; retry")
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

			changed = true
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
			if err := NewNotificationService(txdb).CreateIfAbsent(ctx, &models.NotificationEvent{
				ID: uuid.NewSHA1(subscription.ID, []byte("cancel:admin:"+now.UTC().Format(time.RFC3339Nano))), CustomerID: subscription.CustomerID, EventType: models.NotificationPremiumEnded, CreatedAt: now,
				Data: billing.NotificationData{Reason: string(PremiumEndReasonAdmin), SubscriptionID: billing.SubscriptionID(subscription.ID)},
			}); err != nil {
				return fmt.Errorf("record cancellation notification: %w", err)
			}
			if remote && !deleteHeld {
				return resolveProviderCancelHeld(ctx, txdb, subscription)
			}
			return nil
		}()
		return changed, err
	}); err != nil {
		return err
	}
	return nil
}

// CancellationCommand is the immutable revision-bound command shared by ordinary and finding-driven cancellation.
func CancellationCommand(ctx context.Context, sub *models.Subscription, reason string, revokeAccess, accountDeletion bool) (destructive.Command, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return destructive.Command{}, err
	}
	if sub.MerchantID != mid.UUID() {
		return destructive.Command{}, apperr.Conflictf("subscription merchant mismatch")
	}
	key := fmt.Sprintf("cancel:%s:%d:%t", sub.ID, sub.LifecycleRev, revokeAccess)
	return destructive.CommandFromContext(ctx, destructive.CancelSubscription, sub.ID.String(), key, struct {
		Revision                      int64
		Reason                        string
		RevokeAccess, AccountDeletion bool
	}{sub.LifecycleRev, reason, revokeAccess, accountDeletion})
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

// SetDestructiveAdmissions wires the shared staff-command admission boundary.
func (s *AdminSubscriptionService) SetDestructiveAdmissions(a *destructive.Admissions) {
	s.destructiveAdmissions = a
}
