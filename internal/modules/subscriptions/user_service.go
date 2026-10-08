package subscriptions

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/pagination"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
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

// Sentinel errors for subscription operations. The typed ones (#983) carry
// the status and wire code handlers answer with.
var (
	ErrSubscriptionNotFound  = apperr.New(http.StatusNotFound, "subscription_not_found", "subscription not found")
	ErrSubscriptionNotActive = apperr.New(http.StatusConflict, "subscription_not_active", "subscription is not active")
	// ErrCancelUnsupportedOnRail refuses a server-side cancel on a rail that has
	// no cancel operation.
	ErrCancelUnsupportedOnRail = apperr.New(http.StatusBadRequest, "cancel_unsupported_on_rail", "cancel operation not supported for this rail")

	// ErrCustomerActionRequired refuses a cancel only the customer can make:
	// a Solana cancel is an on-chain transaction the customer's wallet signs
	// (POST /v1/me/subscriptions/{id}/cancel answers it as next_action).
	// OpenRails never marks a chain-truth subscription canceled on its own.
	ErrCustomerActionRequired = apperr.New(http.StatusForbidden, billing.CodeCustomerActionRequired,
		"only the customer can cancel this subscription: its wallet signs the cancel")
)

// UserSubscriptionService handles user-facing subscription operations
type UserSubscriptionService struct {
	SubscriptionService *SubscriptionService
	ProductService      *catalog.ProductService
	PriceService        *catalog.PriceService
	PaymentService      *payments.PaymentService
	NotificationService *NotificationService
	EntitlementService  *entitlements.EntitlementService
	// NMIResolver arms store-scoped NMI clients per merchant (#788).
	NMIResolver NMIClientSource
	clock       clockwork.Clock

	// providerCancel queues the durable provider cancel of a membership the
	// member ends (injected post-construction, after the intent ledger).
	providerCancel ProviderCancelScheduler
}

// ProviderCancelScheduler queues the durable cancel of a provider-billed
// schedule (#1102): the NMI delete, the CCBill DataLink cancel, the Stripe
// cancel. WithTx rebinds it onto the caller's transaction so the intent
// commits atomically with the local cancellation that needs it.
type ProviderCancelScheduler interface {
	// ScheduleProviderCancel queues sub's provider cancel (a no-op when no
	// provider bills it). An NMI delete is due at sub.DeletionScheduledAt,
	// else now, and stamps the marker.
	ScheduleProviderCancel(ctx context.Context, sub *models.Subscription, now time.Time) error
	ScheduleNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID, runAt time.Time) error
	CancelNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID) error
	WithTx(tx pgx.Tx) ProviderCancelScheduler
}

// SetProviderCancelScheduler injects the provider-cancel scheduler.
func (s *UserSubscriptionService) SetProviderCancelScheduler(c ProviderCancelScheduler) {
	s.providerCancel = c
}

// SetClock sets the clock for this service. Used for testing.
func (s *UserSubscriptionService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *UserSubscriptionService) Clock() clockwork.Clock {
	return s.clock
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *UserSubscriptionService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// UserSubscriptionResponse is a customer's subscription with the catalog rows
// its wire projection needs. The HTTP layer serves it as billing.Subscription.
type UserSubscriptionResponse struct {
	*models.Subscription
	// EvaluatedAt binds derived eligibility flags to the service's business clock.
	EvaluatedAt      time.Time
	Price            *models.Price
	ScheduledPrice   *models.Price
	ScheduledProduct *models.Product
	Access           *billing.SubscriptionAccess
}

// EvaluationTime defaults only for responses constructed without a service.
// Service reads stamp EvaluatedAt before either HTTP or library serialization.
func (r *UserSubscriptionResponse) EvaluationTime() time.Time {
	if !r.EvaluatedAt.IsZero() {
		return r.EvaluatedAt.UTC()
	}
	return time.Now().UTC()
}

// GetUserSubscription retrieves the current subscription for a user with enriched data
func (s *UserSubscriptionService) GetUserSubscription(ctx context.Context, userID string) (*UserSubscriptionResponse, error) {
	subscription, err := s.SubscriptionService.GetActiveSubscription(ctx, userID)
	switch {
	case err == nil:
		resp := &UserSubscriptionResponse{Subscription: subscription}
		if err := s.enrichSubscriptionResponses(ctx, []*UserSubscriptionResponse{resp}); err != nil {
			return nil, err
		}
		return resp, nil
	case db.IsNotFound(err):
		access, accessErr := s.activeEntitlementAccess(ctx, userID)
		if accessErr != nil {
			return nil, accessErr
		}
		if access != nil {
			return &UserSubscriptionResponse{Access: access}, nil
		}
		return nil, sql.ErrNoRows
	default:
		return nil, fmt.Errorf("failed to get subscription: %w", err)
	}
}

// GetUserAccessStatus composes all active access grants (subscriptions + entitlements) for a user.
func (s *UserSubscriptionService) GetUserAccessStatus(ctx context.Context, userID string) ([]*billing.SubscriptionAccess, error) {
	grants, err := s.entitlementAccessGrants(ctx, userID)
	if err != nil {
		return nil, err
	}

	if len(grants) == 0 {
		return nil, sql.ErrNoRows
	}
	return grants, nil
}

// GetUserSubscriptionByID retrieves a subscription by ID with ownership verification and enriched data
func (s *UserSubscriptionService) GetUserSubscriptionByID(ctx context.Context, userID string, subscriptionID uuid.UUID) (*UserSubscriptionResponse, error) {
	subscription, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, fmt.Errorf("failed to get subscription: %w", err)
	}

	// Verify ownership
	if subscription.CustomerID.String() != userID {
		return nil, ErrSubscriptionNotFound // Return not found to avoid leaking existence
	}

	resp := &UserSubscriptionResponse{
		Subscription: subscription,
	}

	if err := s.enrichSubscriptionResponses(ctx, []*UserSubscriptionResponse{resp}); err != nil {
		return nil, err
	}

	return resp, nil
}

// ListUserSubscriptions is one page of the customer's own subscriptions
// matching f, newest first.
func (s *UserSubscriptionService) ListUserSubscriptions(ctx context.Context, userID string, f GetSubscriptionsFilters, page billing.PageRequest) (billing.ListPage[*UserSubscriptionResponse], error) {
	customer, err := billing.ParseCustomerID(userID)
	if err != nil || customer.IsZero() {
		return billing.ListPage[*UserSubscriptionResponse]{}, ErrSubscriptionNotFound
	}
	f.CustomerID = customer
	subs, err := s.SubscriptionService.ListSubscribers(ctx, f, page)
	if err != nil {
		return billing.ListPage[*UserSubscriptionResponse]{}, fmt.Errorf("failed to list subscriptions: %w", err)
	}
	out := pagination.Map(subs, func(sub *models.Subscription) *UserSubscriptionResponse {
		return &UserSubscriptionResponse{Subscription: sub}
	})
	if err := s.enrichSubscriptionResponses(ctx, out.Items); err != nil {
		return billing.ListPage[*UserSubscriptionResponse]{}, err
	}
	return out, nil
}

// enrichSubscriptionResponses loads every current and scheduled price, with its
// product, in one batch; access uses one batch of current entitlement windows.
func (s *UserSubscriptionService) enrichSubscriptionResponses(ctx context.Context, responses []*UserSubscriptionResponse) error {
	at := s.now().UTC()
	if s.EntitlementService != nil && len(responses) > 0 {
		customers := make([]uuid.UUID, 0, len(responses))
		seen := make(map[uuid.UUID]bool, len(responses))
		for _, response := range responses {
			if !seen[response.CustomerID] {
				customers = append(customers, response.CustomerID)
				seen[response.CustomerID] = true
			}
		}
		active, err := s.EntitlementService.ListActiveRecordsByCustomers(ctx, customers, at)
		if err != nil {
			return fmt.Errorf("load subscription access: %w", err)
		}
		applySubscriptionAccess(responses, active)
	}
	ids := make([]uuid.UUID, 0, 2*len(responses))
	for _, resp := range responses {
		resp.EvaluatedAt = at
		if resp.Subscription.PriceID != uuid.Nil {
			ids = append(ids, resp.Subscription.PriceID)
		}
		if resp.Subscription.ScheduledPriceID != nil {
			ids = append(ids, *resp.Subscription.ScheduledPriceID)
		}
	}
	if s.PriceService == nil {
		return nil
	}
	prices, err := s.PriceService.GetWithProductByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("failed to load subscription prices: %w", err)
	}
	for _, resp := range responses {
		if price := prices[resp.Subscription.PriceID]; price != nil {
			resp.Price = price
			resp.Subscription.Product = price.Product
		}
		if resp.Subscription.ScheduledPriceID != nil {
			if price := prices[*resp.Subscription.ScheduledPriceID]; price != nil {
				resp.ScheduledPrice = price
				resp.ScheduledProduct = price.Product
			}
		}
	}
	return nil
}

// CancelUserSubscription cancels the member's named subscription at period
// end: access stays to the paid period end and a provider schedule is deleted
// through its durable intent before the next billing.
func (s *UserSubscriptionService) CancelUserSubscription(ctx context.Context, userID string, subscriptionID uuid.UUID, feedback string) error {
	subscription, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			return ErrSubscriptionNotFound
		}
		return fmt.Errorf("load subscription: %w", err)
	}
	if payer := identity.CustomerIDFromString(userID); payer.IsZero() || subscription.CustomerID != payer.UUID() {
		return ErrSubscriptionNotFound
	}
	if !providerCancellable(subscription.Status) {
		return ErrSubscriptionNotActive
	}

	if subscription.CollectionPolicy == models.CollectionPolicyEngine {
		lifecycle := NewSubscriptionLifecycleService(s.SubscriptionService.Database(), s.ProductService, s.PriceService, s.EntitlementService, s.NotificationService, s.PaymentService, s.Clock())
		return lifecycle.CancelMembership(ctx, &CancelMembershipParams{SubscriptionID: &subscription.ID, CancelType: models.CancelTypeUser, CancelFeedback: &feedback})
	}

	now := s.now()

	// enqueueRemoteIntent commits the rail's durable remote-mutation intent in
	// the SAME transaction as the local cancellation (nil = no remote intent).
	var enqueueRemoteIntent func(ctx context.Context, tx pgx.Tx) error

	switch {
	case rails.IsNMI(subscription.Rail):
		// The NMI delete always rides the durable nmi_delete_subscription
		// intent, committed with the cancellation: the destructive switch,
		// volume breaker and verify-then-execute apply exactly as for an admin
		// cancel. With a genuine undo window (issue 216) it is due at
		// period_end - margin, keeping the schedule for a resume; otherwise now.
		if subscription.RailSubscriptionID != "" {
			if s.providerCancel == nil {
				return fmt.Errorf("nmi remote-delete scheduler unavailable")
			}
			if _, err := RequireProviderCancelArmed(ctx, s.SubscriptionService.Database(), subscription, false); err != nil {
				return err
			}
			deleteAt, deferred := NMIDeferredDeleteAt(subscription, now)
			if !deferred {
				deleteAt = now
			}
			subscription.DeletionScheduledAt = &deleteAt
			enqueueRemoteIntent = func(ctx context.Context, tx pgx.Tx) error {
				if err := resolveProviderCancelHeld(ctx, db.NewWithPgxTx(tx), subscription); err != nil {
					return err
				}
				return s.providerCancel.WithTx(tx).ScheduleProviderCancel(ctx, subscription, now)
			}
		}
	case subscription.Rail == models.RailCCBill:
		// #696: merchant-initiated CCBill cancel via DataLink SMS — same
		// semantics as NMI: local cancel with the #691 paid runway plus a
		// durable remote-cancel intent, atomic in the cancel tx. CCBill's
		// cancelSubscription stops rebilling and keeps access through the paid
		// period on its own side, so the intent is due immediately (no undo
		// window to defer for) and the cancel is not resumable.
		if s.providerCancel == nil {
			return fmt.Errorf("ccbill remote-cancel scheduler unavailable")
		}
		enqueueRemoteIntent = func(ctx context.Context, tx pgx.Tx) error {
			return s.providerCancel.WithTx(tx).ScheduleProviderCancel(ctx, subscription, now)
		}
	case subscription.Rail == models.RailSolana:
		return ErrCustomerActionRequired
	default:
		return fmt.Errorf("unable to cancel subscription for rail %s", subscription.Rail)
	}

	// Persist the cancellation; any durable remote intent (deferred NMI delete,
	// CCBill remote cancel) is enqueued IN THE SAME TRANSACTION.
	if err := s.SubscriptionService.Database().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := db.NewWithPgxTx(tx)
		// Decide on the locked row, never on the snapshot read above.
		locked, err := NewSubscriptionRepo(txdb).GetByIDForUpdate(ctx, subscription.ID)
		if err != nil {
			return fmt.Errorf("lock subscription: %w", err)
		}
		if !providerCancellable(locked.Status) {
			return ErrSubscriptionNotActive
		}
		if _, err := Transition(locked, lifecycle.Cancel{Kind: lifecycle.CancelUser, At: now}, now); err != nil {
			return fmt.Errorf("cancel subscription %s: %w", locked.ID, err)
		}
		if feedback != "" {
			locked.CancelFeedback = &feedback
		}
		locked.DeletionScheduledAt = subscription.DeletionScheduledAt
		if err := NewSubscriptionRepo(txdb).UpdateAt(ctx, locked, now); err != nil {
			return fmt.Errorf("failed to update subscription status: %w", err)
		}
		subscription = locked
		txEntSvc := entitlements.NewEntitlementService(txdb, s.clock)
		if err := txEntSvc.RevokeSourcesForSubscription(ctx, subscription.CustomerID.String(), subscription.ID, models.EntitlementRevokeSuperseded, models.EntitlementSourceGrace); err != nil {
			return fmt.Errorf("failed to bound subscription access windows: %w", err)
		}
		if enqueueRemoteIntent != nil {
			return enqueueRemoteIntent(ctx, tx)
		}
		return nil
	}); err != nil {
		if enqueueRemoteIntent != nil {
			return fmt.Errorf("failed to persist cancellation with remote intent: %w", err)
		}
		return err
	}

	// Add notification
	notification := &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: identity.CustomerIDFromString(userID).UUID(),
		EventType:  models.NotificationPremiumEnded,
		Data:       billing.NotificationData{Reason: string(PremiumEndReasonUserCancel)},
	}
	if err := s.NotificationService.Create(ctx, notification); err != nil {
		log.WithFields(log.Fields{
			"subscription_id":   subscription.ID,
			"user_id":           userID,
			"notification_type": notification.EventType,
			"error":             err.Error(),
		}).Error("Failed to create notification during subscription cancellation")
	}

	return nil
}

// applySubscriptionAccess projects live entitlement windows, never billing status
// or a price snapshot. A canceled membership may retain bought access; a live
// membership may have no access between its independently scheduled grants.
func applySubscriptionAccess(responses []*UserSubscriptionResponse, active map[uuid.UUID][]models.Entitlement) {
	bySource := make(map[[2]uuid.UUID]*billing.SubscriptionAccess)
	for customer, ents := range active {
		for _, ent := range ents {
			if ent.SourceID == nil || (ent.SourceType != models.EntitlementSourceSubscription && ent.SourceType != models.EntitlementSourceGrace) {
				continue
			}
			grant := entitlementAccess(ent)
			if grant == nil {
				continue
			}
			key := [2]uuid.UUID{customer, *ent.SourceID}
			if current := bySource[key]; current != nil && (current.EndsAt == nil || grant.EndsAt != nil && !grant.EndsAt.After(*current.EndsAt)) {
				continue
			}
			bySource[key] = grant
		}
	}
	for _, response := range responses {
		response.Access = nil
		if grant := bySource[[2]uuid.UUID{response.CustomerID, response.ID}]; grant != nil {
			copy := *grant
			copy.Kind, copy.Rail = "subscription", string(response.Rail)
			response.Access = &copy
		}
	}
}

func entitlementAccess(ent models.Entitlement) *billing.SubscriptionAccess {
	if ent.Entitlement == "" {
		return nil
	}
	grant := &billing.SubscriptionAccess{Kind: "entitlement", Entitlement: ent.Entitlement, SourceType: string(ent.SourceType), StartsAt: ent.StartsAt, EndsAt: ent.EndsAt}
	if ent.SourceID != nil {
		grant.SourceID = billing.SourceRef(string(ent.SourceType), ent.SourceID.String())
		if ent.SourceType == models.EntitlementSourceSubscription || ent.SourceType == models.EntitlementSourceGrace {
			grant.SubscriptionID = billing.SubscriptionID(*ent.SourceID)
		}
	}
	return grant
}

func (s *UserSubscriptionService) activeEntitlementAccess(ctx context.Context, userID string) (*billing.SubscriptionAccess, error) {
	grants, err := s.entitlementAccessGrants(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(grants) > 0 {
		return grants[0], nil
	}
	return nil, nil
}

func (s *UserSubscriptionService) entitlementAccessGrants(ctx context.Context, userID string) ([]*billing.SubscriptionAccess, error) {
	if s.EntitlementService == nil {
		return nil, nil
	}
	ents, err := s.EntitlementService.ListActiveRecords(ctx, userID, s.now())
	if err != nil {
		return nil, fmt.Errorf("failed to list entitlements: %w", err)
	}
	grants := make([]*billing.SubscriptionAccess, 0, len(ents))
	for _, ent := range ents {
		if grant := entitlementAccess(ent); grant != nil {
			grants = append(grants, grant)
		}
	}
	return grants, nil
}

// NewUserSubscriptionService creates a new UserSubscriptionService
func NewUserSubscriptionService(
	subscriptionService *SubscriptionService,
	productService *catalog.ProductService,
	priceService *catalog.PriceService,
	paymentService *payments.PaymentService,
	notificationService *NotificationService,
	entitlementService *entitlements.EntitlementService,
	nmiResolver NMIClientSource,
	clocks ...clockwork.Clock,
) *UserSubscriptionService {
	return &UserSubscriptionService{
		NMIResolver:         nmiResolver,
		SubscriptionService: subscriptionService,
		ProductService:      productService,
		PriceService:        priceService,
		PaymentService:      paymentService,
		NotificationService: notificationService,
		EntitlementService:  entitlementService,
		clock:               timeutil.FirstClock(clocks...),
	}
}
