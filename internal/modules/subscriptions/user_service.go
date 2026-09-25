package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/billing/lifecycle"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
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
	"github.com/open-rails/openrails/pkg/query"
	log "github.com/sirupsen/logrus"
)

// Sentinel errors for subscription operations. The typed ones (#983) carry
// the status and wire code handlers answer with.
var (
	ErrSubscriptionNotFound  = apperr.New(http.StatusNotFound, "subscription_not_found", "subscription not found")
	ErrSubscriptionNotActive = apperr.New(http.StatusConflict, "subscription_not_active", "subscription is not active")
	// ErrCancelUnsupportedOnRail refuses a server-side cancel on a rail that has
	// no cancel operation.
	ErrCancelUnsupportedOnRail  = apperr.New(http.StatusBadRequest, "cancel_unsupported_on_rail", "cancel operation not supported for this rail")
	ErrNotificationNotFound     = errors.New("notification not found")
	ErrNotificationAccessDenied = errors.New("notification does not belong to user")

	// ErrSolanaCancelNeedsWalletSignature refuses the rail-agnostic cancel on
	// Solana (or#896). A Solana cancel is an on-chain transaction the
	// SUBSCRIBER'S wallet must sign — OpenRails holds no authority to revoke
	// the delegation and never DB-only "soft cancels" a chain-truth
	// subscription. This used to fall through the worker's default branch into
	// a facade with no Solana case and fail permanently, which read as a bug
	// rather than a rail fact.
	ErrSolanaCancelNeedsWalletSignature = apperr.New(http.StatusBadRequest, "solana_cancel_needs_wallet_signature",
		"cancelling a Solana subscription requires the subscriber's wallet signature: "+
			"POST /v1/me/subscriptions/{id}/solana-cancel-tx to build the unsigned cancel_subscription "+
			"transaction, sign and send it from the wallet, then POST /v1/me/subscriptions/{id}/solana-cancel "+
			"with the signature to confirm and mirror it")
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

	// deferDelete enqueues a deferred NMI delete_subscription job (issue 216).
	// When nil, NMI cancellations fall back to deleting inline immediately. It is
	// injected post-construction (the River producer is built after services).
	deferDelete DeferredDeleteScheduler

	// ccbillCancel enqueues the durable ccbill_cancel_subscription intent
	// (#696). Injected post-construction like deferDelete.
	ccbillCancel CCBillRemoteCancelScheduler
}

// DeferredDeleteScheduler schedules an NMI delete_subscription intent to run
// at a future time (#358 ledger). WithTx rebinds the scheduler onto a caller
// transaction so the intent enqueue COMMITS ATOMICALLY with the subscription
// update that stamps the DeletionScheduledAt marker — the marker<->intent
// invariant holds transactionally; there is no crash window between them.
type DeferredDeleteScheduler interface {
	ScheduleNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID, runAt time.Time) error
	CancelNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID) error
	WithTx(tx pgx.Tx) DeferredDeleteScheduler
}

// SetDeferredDeleteScheduler injects the deferred-delete scheduler. Wired in
// build_runtime after the River producer exists.
func (s *UserSubscriptionService) SetDeferredDeleteScheduler(d DeferredDeleteScheduler) {
	s.deferDelete = d
}

// CCBillRemoteCancelScheduler schedules a durable ccbill_cancel_subscription
// intent (#696) — merchant-initiated cancel through the DataLink SMS choke
// point. WithTx rebinds onto the caller's transaction so the intent enqueue
// COMMITS ATOMICALLY with the local cancellation (queue-always, #679).
type CCBillRemoteCancelScheduler interface {
	ScheduleCCBillCancel(ctx context.Context, userID string, subscriptionID uuid.UUID) error
	WithTx(tx pgx.Tx) CCBillRemoteCancelScheduler
}

// SetCCBillCancelScheduler injects the CCBill remote-cancel scheduler
// (intents.CCBillCancelScheduler, wired in build_runtime).
func (s *UserSubscriptionService) SetCCBillCancelScheduler(c CCBillRemoteCancelScheduler) {
	s.ccbillCancel = c
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
// its wire projection needs. The HTTP layer serves it as openrails.Subscription.
type UserSubscriptionResponse struct {
	*models.Subscription
	// EvaluatedAt binds derived eligibility flags to the service's business clock.
	EvaluatedAt      time.Time
	Price            *models.Price
	ScheduledPrice   *models.Price
	ScheduledProduct *models.Product
	Access           *openrails.SubscriptionAccess
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
		resp := &UserSubscriptionResponse{Subscription: subscription, Access: accessFromSubscription(subscription)}
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
func (s *UserSubscriptionService) GetUserAccessStatus(ctx context.Context, userID string) ([]*openrails.SubscriptionAccess, error) {
	grants := make([]*openrails.SubscriptionAccess, 0, 2)
	skipSubscriptionIDs := make(map[uuid.UUID]struct{})
	if s.SubscriptionService != nil {
		if sub, err := s.SubscriptionService.GetActiveSubscription(ctx, userID); err == nil {
			grants = append(grants, accessFromSubscription(sub))
			skipSubscriptionIDs[sub.ID] = struct{}{}
		} else if err != nil && !db.IsNotFound(err) {
			return nil, fmt.Errorf("failed to fetch subscription access: %w", err)
		}
	}
	ents, err := s.entitlementAccessGrants(ctx, userID, skipSubscriptionIDs)
	if err != nil {
		return nil, err
	}
	grants = append(grants, ents...)
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
		Access:       accessFromSubscription(subscription),
	}

	if err := s.enrichSubscriptionResponses(ctx, []*UserSubscriptionResponse{resp}); err != nil {
		return nil, err
	}

	return resp, nil
}

// GetUserSubscriptionHistory retrieves subscription history for a user
func (s *UserSubscriptionService) GetUserSubscriptionHistory(ctx context.Context, userID string, queryOpts *query.QueryOptions[GetSubscriptionsFilters]) ([]*UserSubscriptionResponse, int64, error) {
	if queryOpts.Filters.CustomerID == "" {
		queryOpts.Filters.CustomerID = userID
	}

	subscriptions, total, err := s.SubscriptionService.GetSubscribers(ctx, *queryOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get subscription history: %w", err)
	}
	queryOpts.SetTotal(total)

	responses := make([]*UserSubscriptionResponse, len(subscriptions))
	for i, sub := range subscriptions {
		responses[i] = &UserSubscriptionResponse{Subscription: sub, Access: accessFromSubscription(sub)}
	}
	if err := s.enrichSubscriptionResponses(ctx, responses); err != nil {
		return nil, 0, err
	}

	return responses, total, nil
}

// enrichSubscriptionResponses loads every current and scheduled price, with its
// product, in one batch.
func (s *UserSubscriptionService) enrichSubscriptionResponses(ctx context.Context, responses []*UserSubscriptionResponse) error {
	ids := make([]uuid.UUID, 0, 2*len(responses))
	for _, resp := range responses {
		resp.EvaluatedAt = s.now().UTC()
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

// GetUserPayments retrieves one-off purchases for a user
func (s *UserSubscriptionService) GetUserPayments(ctx context.Context, userID string, queryOpts *query.QueryOptions[payments.GetPaymentsFilters]) ([]*models.Payment, int64, error) {
	if queryOpts.Filters.CustomerID == "" {
		queryOpts.Filters.CustomerID = userID
	}

	purchases, total, err := s.PaymentService.GetPayments(ctx, *queryOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get purchases: %w", err)
	}
	queryOpts.SetTotal(total)

	return purchases, total, nil
}

// GetUserNotifications retrieves notifications for a user
func (s *UserSubscriptionService) GetUserNotifications(ctx context.Context, userID string, queryOpts *query.QueryOptions[GetNotificationsFilters]) ([]*models.NotificationQueue, int64, error) {
	// Set user filter
	if queryOpts.Filters.UserID == "" {
		queryOpts.Filters.UserID = userID
	}

	notifications, total, err := s.NotificationService.GetNotifications(ctx, *queryOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get notifications: %w", err)
	}
	queryOpts.SetTotal(total)

	return notifications, total, nil
}

// MarkNotificationRead marks a notification as read
func (s *UserSubscriptionService) MarkNotificationRead(ctx context.Context, userID string, notificationID uuid.UUID) error {
	notification, err := s.NotificationService.GetByID(ctx, notificationID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotificationNotFound, err)
	}

	// Verify the notification belongs to the user
	if notification.CustomerID.String() != userID {
		return ErrNotificationAccessDenied
	}

	return s.NotificationService.MarkAsSeen(ctx, notificationID, notification.CustomerID)
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
			if s.deferDelete == nil {
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
				return s.deferDelete.WithTx(tx).ScheduleNMIDelete(ctx, userID, subscription.ID, deleteAt)
			}
		}
	case subscription.Rail == models.RailCCBill:
		// #696: merchant-initiated CCBill cancel via DataLink SMS — same
		// semantics as NMI: local cancel with the #691 paid runway plus a
		// durable remote-cancel intent, atomic in the cancel tx. CCBill's
		// cancelSubscription stops rebilling and keeps access through the paid
		// period on its own side, so the intent is due immediately (no undo
		// window to defer for) and the cancel is not resumable.
		if s.ccbillCancel == nil {
			return fmt.Errorf("ccbill remote-cancel scheduler unavailable")
		}
		enqueueRemoteIntent = func(ctx context.Context, tx pgx.Tx) error {
			return s.ccbillCancel.WithTx(tx).ScheduleCCBillCancel(ctx, userID, subscription.ID)
		}
	case subscription.Rail == models.RailSolana:
		return ErrSolanaCancelNeedsWalletSignature
	default:
		return fmt.Errorf("unable to cancel subscription for rail %s", subscription.Rail)
	}

	// #691 closure: a user cancel is PROOF — write the access end on disk NOW, at
	// the known period end (resumable runway; a dead system cannot extend a
	// cancelled sub). Immediate when no future paid period remains.
	accessEnd := now
	if subscription.CurrentPeriodEndsAt != nil && subscription.CurrentPeriodEndsAt.After(now) {
		accessEnd = *subscription.CurrentPeriodEndsAt
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
		if err := txEntSvc.BoundSubscriptionAccess(ctx, subscription.ID, accessEnd); err != nil {
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
		Data:       openrails.NotificationData{Reason: string(PremiumEndReasonUserCancel)},
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

func accessFromSubscription(sub *models.Subscription) *openrails.SubscriptionAccess {
	grant := &openrails.SubscriptionAccess{
		Kind:           "subscription",
		Entitlement:    "premium",
		Rail:           string(sub.Rail),
		SubscriptionID: openrails.SubscriptionID(sub.ID),
		StartAt:        sub.StartedAt,
	}
	if sub.CurrentPeriodStartsAt != nil && !sub.CurrentPeriodStartsAt.IsZero() {
		grant.StartAt = *sub.CurrentPeriodStartsAt
	}
	if sub.CurrentPeriodEndsAt != nil && !sub.CurrentPeriodEndsAt.IsZero() {
		grant.EndAt = sub.CurrentPeriodEndsAt
	}
	return grant
}

func (s *UserSubscriptionService) activeEntitlementAccess(ctx context.Context, userID string) (*openrails.SubscriptionAccess, error) {
	grants, err := s.entitlementAccessGrants(ctx, userID, nil)
	if err != nil {
		return nil, err
	}
	if len(grants) > 0 {
		return grants[0], nil
	}
	return nil, nil
}

func (s *UserSubscriptionService) entitlementAccessGrants(ctx context.Context, userID string, skipSubs map[uuid.UUID]struct{}) ([]*openrails.SubscriptionAccess, error) {
	if s.EntitlementService == nil {
		return nil, nil
	}
	ents, err := s.EntitlementService.ListActiveRecords(ctx, userID, s.now())
	if err != nil {
		return nil, fmt.Errorf("failed to list entitlements: %w", err)
	}
	grants := make([]*openrails.SubscriptionAccess, 0, len(ents))
	for _, ent := range ents {
		if ent.Entitlement == "" {
			continue
		}
		fromSubscription := ent.SourceType == models.EntitlementSourceSubscription && ent.SourceID != nil
		if fromSubscription {
			if _, ok := skipSubs[*ent.SourceID]; ok {
				continue
			}
		}
		grant := &openrails.SubscriptionAccess{
			Kind:        "entitlement",
			Entitlement: ent.Entitlement,
			SourceType:  string(ent.SourceType),
			StartAt:     ent.StartAt,
			EndAt:       ent.EndAt,
		}
		if ent.SourceID != nil {
			grant.SourceID = openrails.SourceRef(string(ent.SourceType), ent.SourceID.String())
		}
		if fromSubscription {
			grant.SubscriptionID = openrails.SubscriptionID(*ent.SourceID)
		}
		grants = append(grants, grant)
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
