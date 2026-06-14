package subscriptions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	openrailsdb "github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/query"
	log "github.com/sirupsen/logrus"
)

// Sentinel errors for subscription operations
var (
	ErrSubscriptionNotFound     = errors.New("subscription not found")
	ErrSubscriptionNotActive    = errors.New("subscription is not active")
	ErrNotificationNotFound     = errors.New("notification not found")
	ErrNotificationAccessDenied = errors.New("notification does not belong to user")
)

// UserSubscriptionService handles user-facing subscription operations
type UserSubscriptionService struct {
	SubscriptionService *SubscriptionService
	ProductService      *catalog.ProductService
	PriceService        *catalog.PriceService
	PaymentService      *payments.PaymentService
	NotificationService NotificationStore
	EntitlementService  *entitlements.EntitlementService
	NMIClients          map[string]*nmi.NMIClient
	clock               clockwork.Clock

	// deferDelete enqueues a deferred NMI delete_subscription job (issue 216).
	// When nil, NMI cancellations fall back to deleting inline immediately. It is
	// injected post-construction (the River producer is built after services).
	deferDelete DeferredDeleteScheduler
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

// UserSubscriptionResponse represents a user's subscription with enriched data
type UserSubscriptionResponse struct {
	*models.Subscription
	Price            *models.Price    `json:"-"`
	ScheduledPrice   *models.Price    `json:"-"`
	ScheduledProduct *models.Product  `json:"-"`
	Access           *UserAccessGrant `json:"access,omitempty"`
}

// MarshalJSON keeps user-facing subscription payloads on the same Stripe-like
// resource contract as catalog endpoints while preserving model pointers for
// internal service callers.
func (r *UserSubscriptionResponse) MarshalJSON() ([]byte, error) {
	type userSubscriptionJSON struct {
		ID                    uuid.UUID                 `json:"id,omitempty"`
		MerchantSubjectID       string                    `json:"tenant_subject_id,omitempty"`
		ProductID             uuid.UUID                 `json:"product_id,omitempty"`
		PriceID               uuid.UUID                 `json:"price_id,omitempty"`
		ScheduledPriceID      *uuid.UUID                `json:"scheduled_price_id,omitempty"`
		Status                models.SubscriptionStatus `json:"status,omitempty"`
		StartedAt             time.Time                 `json:"started_at,omitempty"`
		EndedAt               *time.Time                `json:"ended_at,omitempty"`
		CurrentPeriodStartsAt *time.Time                `json:"current_period_starts_at,omitempty"`
		CurrentPeriodEndsAt   *time.Time                `json:"current_period_ends_at,omitempty"`
		Processor             models.Processor          `json:"processor,omitempty"`
		CancelFeedback        *string                   `json:"cancel_feedback,omitempty"`
		CancelType            *models.CancelType        `json:"cancel_type,omitempty"`
		CancelledAt           *time.Time                `json:"cancelled_at,omitempty"`
		Resumable             bool                      `json:"resumable"`
		CancelScheduled       bool                      `json:"cancel_scheduled"`
		CancelMode            string                    `json:"cancel_mode,omitempty"`
		CancelPortalURL       *string                   `json:"cancel_portal_url,omitempty"`
		CreatedAt             time.Time                 `json:"created_at,omitempty"`
		UpdatedAt             time.Time                 `json:"updated_at,omitempty"`
		Price                 *api.PriceObject          `json:"price,omitempty"`
		Product               *api.ProductObject        `json:"product,omitempty"`
		ScheduledPrice        *api.PriceObject          `json:"scheduled_price,omitempty"`
		ScheduledProduct      *api.ProductObject        `json:"scheduled_product,omitempty"`
		Card                  *subscriptionCardJSON     `json:"card,omitempty"`
		Access                *UserAccessGrant          `json:"access,omitempty"`
	}

	if r.Subscription != nil {
		// Resumability surface — derived from the single shared predicate so the
		// HTTP serialization stays in lockstep with the resume handler, the worker,
		// and the library DTO.
		now := time.Now().UTC()
		out := userSubscriptionJSON{
			ID:                    r.Subscription.ID,
			MerchantSubjectID:       r.Subscription.MerchantSubjectID.String(),
			ProductID:             r.Subscription.ProductID,
			PriceID:               r.Subscription.PriceID,
			ScheduledPriceID:      r.Subscription.ScheduledPriceID,
			Status:                r.Subscription.Status,
			StartedAt:             r.Subscription.StartedAt,
			EndedAt:               r.Subscription.EndedAt,
			CurrentPeriodStartsAt: r.Subscription.CurrentPeriodStartsAt,
			CurrentPeriodEndsAt:   r.Subscription.CurrentPeriodEndsAt,
			Processor:             r.Subscription.Processor,
			CancelFeedback:        r.Subscription.CancelFeedback,
			CancelType:            r.Subscription.CancelType,
			CancelledAt:           r.Subscription.CancelledAt,
			Resumable:             Resumable(r.Subscription, now),
			CancelScheduled:       CancelScheduled(r.Subscription, now),
			CancelMode:            string(CancelModeFor(r.Subscription, now)),
			CancelPortalURL:       CancelPortalURL(r.Subscription, now),
			CreatedAt:             r.Subscription.CreatedAt,
			UpdatedAt:             r.Subscription.UpdatedAt,
			Access:                r.Access,
		}
		if r.Price != nil {
			price := priceToAPIObject(r.Price)
			out.Price = &price
		}
		if r.Subscription.Product != nil {
			product := productToAPIObject(r.Subscription.Product)
			out.Product = &product
		}
		if r.ScheduledPrice != nil {
			price := priceToAPIObject(r.ScheduledPrice)
			out.ScheduledPrice = &price
		}
		if r.ScheduledProduct != nil {
			product := productToAPIObject(r.ScheduledProduct)
			out.ScheduledProduct = &product
		}
		out.Card = subscriptionCardFromPaymentMethod(r.Subscription.PaymentMethod)
		return json.Marshal(out)
	}

	return json.Marshal(userSubscriptionJSON{Access: r.Access})
}

// subscriptionCardJSON is the card on the subscription's current payment method,
// served purely from the DB (the linked openrails.payment_methods row). No Stripe
// fetch.
type subscriptionCardJSON struct {
	Brand    string `json:"brand,omitempty"`
	Last4    string `json:"last4,omitempty"`
	ExpMonth *int   `json:"exp_month,omitempty"`
	ExpYear  *int   `json:"exp_year,omitempty"`
}

func subscriptionCardFromPaymentMethod(pm *models.PaymentMethod) *subscriptionCardJSON {
	if pm == nil {
		return nil
	}
	brand, last4 := "", ""
	if pm.CardType != nil {
		brand = *pm.CardType
	}
	if pm.LastFour != nil {
		last4 = *pm.LastFour
	}
	if brand == "" && last4 == "" {
		return nil
	}
	card := &subscriptionCardJSON{Brand: brand, Last4: last4}
	if pm.ExpiryDate != nil {
		if month, year, ok := sharedformat.ParseExpiry(*pm.ExpiryDate); ok {
			card.ExpMonth = &month
			card.ExpYear = &year
		}
	}
	return card
}

func priceToAPIObject(p *models.Price) api.PriceObject {
	var recurring *api.RecurringInfo
	if p.BillingCycleDays != nil && *p.BillingCycleDays > 0 {
		interval, intervalCount := sharedformat.BillingCycleDaysToInterval(*p.BillingCycleDays)
		recurring = &api.RecurringInfo{Interval: interval, IntervalCount: intervalCount}
	}
	priceType := "one_time"
	if recurring != nil {
		priceType = "recurring"
	}
	return api.PriceObject{
		ID:         api.FormatPriceID(p.ID),
		Object:     "price",
		UnitAmount: p.Amount,
		Currency:   p.Currency,
		Type:       priceType,
		Recurring:  recurring,
		Product:    api.FormatProductID(p.ProductID),
		Active:     p.IsPurchasable(),
		Livemode:   false,
		Metadata:   map[string]string{},
		Created:    api.ToUnix(p.CreatedAt),
	}
}

func productToAPIObject(p *models.Product) api.ProductObject {
	return api.ProductObject{
		ID:               api.FormatProductID(p.ID),
		Object:           "product",
		Slug:             p.Slug,
		Name:             p.DisplayName,
		Description:      p.Description,
		EntitlementsSpec: p.EntitlementsSpec,
		CreditsSpec:      creditsSpecToAPIObject(p.CreditsSpec),
		TierGroup:        p.TierGroup,
		TierRank:         p.TierRank,
		Active:           p.IsPurchasable(),
		Livemode:         false,
		Metadata:         map[string]string{},
		Created:          api.ToUnix(p.CreatedAt),
		Updated:          api.ToUnix(p.UpdatedAt),
	}
}

func creditsSpecToAPIObject(specs models.CreditsSpec) map[string]api.CreditGrantSpecObject {
	if len(specs) == 0 {
		return nil
	}
	out := make(map[string]api.CreditGrantSpecObject, len(specs))
	for creditType, spec := range specs {
		out[creditType] = api.CreditGrantSpecObject{
			Amount:      spec.Amount,
			ExpiresDays: spec.ExpiresDays,
			Cadence:     string(spec.Cadence),
		}
	}
	return out
}

// UserAccessGrant summarizes how the user currently has premium access (subscription vs one-off entitlement).
type UserAccessGrant struct {
	Kind                    string                        `json:"kind"`
	Entitlement             string                        `json:"entitlement"`
	SourceType              *models.EntitlementSourceType `json:"source_type,omitempty"`
	SourceID                *uuid.UUID                    `json:"source_id,omitempty"`
	SubscriptionID          *uuid.UUID                    `json:"subscription_id,omitempty"`
	Processor               string                        `json:"processor,omitempty"`
	ProcessorSubscriptionID *string                       `json:"-"`
	StartAt                 time.Time                     `json:"start_at"`
	EndAt                   *time.Time                    `json:"end_at,omitempty"`
}

// GetUserSubscription retrieves the current subscription for a user with enriched data
func (s *UserSubscriptionService) GetUserSubscription(ctx context.Context, userID string) (*UserSubscriptionResponse, error) {
	subscription, err := s.SubscriptionService.GetActiveSubscription(ctx, userID)
	switch {
	case err == nil:
		resp := &UserSubscriptionResponse{Subscription: subscription, Access: accessFromSubscription(subscription)}
		s.enrichSubscriptionResponse(ctx, resp)
		return resp, nil
	case repo.IsNotFound(err):
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
func (s *UserSubscriptionService) GetUserAccessStatus(ctx context.Context, userID string) ([]*UserAccessGrant, error) {
	grants := make([]*UserAccessGrant, 0, 2)
	skipSubscriptionIDs := make(map[uuid.UUID]struct{})
	if s.SubscriptionService != nil {
		if sub, err := s.SubscriptionService.GetActiveSubscription(ctx, userID); err == nil {
			grants = append(grants, accessFromSubscription(sub))
			skipSubscriptionIDs[sub.ID] = struct{}{}
		} else if err != nil && !repo.IsNotFound(err) {
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
		if repo.IsNotFound(err) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, fmt.Errorf("failed to get subscription: %w", err)
	}

	// Verify ownership
	if subscription.MerchantSubjectID.String() != userID {
		return nil, ErrSubscriptionNotFound // Return not found to avoid leaking existence
	}

	resp := &UserSubscriptionResponse{
		Subscription: subscription,
		Access:       accessFromSubscription(subscription),
	}

	s.enrichSubscriptionResponse(ctx, resp)

	return resp, nil
}

// GetUserSubscriptionHistory retrieves subscription history for a user
func (s *UserSubscriptionService) GetUserSubscriptionHistory(ctx context.Context, userID string, queryOpts *query.QueryOptions[GetSubscriptionsFilters]) ([]*UserSubscriptionResponse, int64, error) {
	// Set user filter
	if queryOpts.Filters.UserID == "" {
		queryOpts.Filters.UserID = userID
	}

	subscriptions, total, err := s.SubscriptionService.GetSubscribers(ctx, *queryOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get subscription history: %w", err)
	}
	queryOpts.SetTotal(total)

	responses := make([]*UserSubscriptionResponse, len(subscriptions))
	for i, sub := range subscriptions {
		responses[i] = &UserSubscriptionResponse{
			Subscription: sub,
		}
		s.enrichSubscriptionResponse(ctx, responses[i])
	}

	return responses, total, nil
}

func (s *UserSubscriptionService) enrichSubscriptionResponse(ctx context.Context, resp *UserSubscriptionResponse) {
	if resp == nil || resp.Subscription == nil {
		return
	}
	if resp.Subscription.PriceID != uuid.Nil && s.PriceService != nil {
		if price, err := s.PriceService.GetByID(ctx, resp.Subscription.PriceID); err == nil {
			resp.Price = price

			if s.ProductService != nil {
				if product, err := s.ProductService.GetByID(ctx, price.ProductID); err == nil {
					resp.Subscription.Product = product
				}
			}
		}
	}
	if resp.Subscription.ScheduledPriceID != nil && *resp.Subscription.ScheduledPriceID != uuid.Nil && s.PriceService != nil {
		if price, err := s.PriceService.GetByID(ctx, *resp.Subscription.ScheduledPriceID); err == nil {
			resp.ScheduledPrice = price

			if s.ProductService != nil {
				if product, err := s.ProductService.GetByID(ctx, price.ProductID); err == nil {
					resp.ScheduledProduct = product
				}
			}
		}
	}
}

// GetUserPayments retrieves one-off purchases for a user
func (s *UserSubscriptionService) GetUserPayments(ctx context.Context, userID string, queryOpts *query.QueryOptions[payments.GetPaymentsFilters]) ([]*models.Payment, int64, error) {
	// Set user filter
	if queryOpts.Filters.UserID == "" {
		queryOpts.Filters.UserID = userID
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
	if notification.MerchantSubjectID.String() != userID {
		return ErrNotificationAccessDenied
	}

	notification.MarkAsSeen() // Mark as seen (new boolean field)
	return s.NotificationService.Update(ctx, notification)
}

// CCBillCancelError is returned when a user tries to cancel a CCBill subscription
// CCBill does not have a public API for merchant-initiated cancellation
type CCBillCancelError struct {
	SupportURL string `json:"support_url"`
	Message    string `json:"message"`
}

func (e *CCBillCancelError) Error() string {
	return e.Message
}

// CancelUserSubscription cancels a user's subscription
func (s *UserSubscriptionService) CancelUserSubscription(ctx context.Context, userID string, feedback string) error {
	subscription, err := s.SubscriptionService.GetActiveSubscription(ctx, userID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSubscriptionNotFound, err)
	}

	// CCBill doesn't have a public API for merchant-initiated cancellation
	// Users must cancel through CCBill's consumer support portal
	if subscription.Processor == models.ProcessorCCBill {
		return &CCBillCancelError{
			SupportURL: "https://support.ccbill.com",
			Message:    "CCBill subscriptions cannot be cancelled through our system. Please visit the CCBill consumer support portal to manage your subscription. You will need the email address you used when subscribing.",
		}
	}

	// Only NMI-backed processors can be cancelled via this service
	if !processors.IsNMIBackedProcessor(subscription.Processor) {
		return fmt.Errorf("unable to cancel subscription for processor %s", subscription.Processor)
	}

	now := s.now()
	provider := strings.ToLower(string(subscription.Processor))

	// Issue 216: when there is a genuine future undo window, DEFER the
	// processor-side delete instead of doing it inline. We keep the NMI
	// subscription alive (so a resume is a no-op processor-side) and schedule
	// delete_subscription to fire at period_end - safety margin. If the window
	// has already opened (now >= period_end - margin, common near rebill) or the
	// period end is unknown/past, we delete IMMEDIATELY exactly as before.
	deleteAt, defer_ := NMIDeferredDeleteAt(subscription, now)
	deferScheduled := defer_ && s.deferDelete != nil
	scheduleKillSwitchedDelete := false
	if deferScheduled {
		// The delete intent is enqueued below, in the SAME transaction as the
		// cancellation update, so marker and intent commit atomically.
		subscription.DeletionScheduledAt = &deleteAt
	} else {
		// Immediate delete with NMI (no scheduled job).
		subscription.DeletionScheduledAt = nil
		if s.NMIClients != nil {
			if client, ok := s.NMIClients[provider]; ok && subscription.ProcessorSubscriptionID != "" {
				if err := client.DeleteRecurringSubscription(subscription.ProcessorSubscriptionID); err != nil {
					if errors.Is(err, nmi.ErrSubscriptionDeletesDisabled) {
						// Kill switch: cancel locally but keep a deferred-delete marker so
						// the pending remote delete stays discoverable for replay or
						// reconciliation once deletions are re-enabled.
						log.WithFields(log.Fields{
							"subscription_id": subscription.ID,
							"processor":       provider,
						}).Warn("processor subscription deletes disabled; cancelling locally, remote subscription left for reconciliation")
						subscription.DeletionScheduledAt = &now
						scheduleKillSwitchedDelete = true
					} else {
						return fmt.Errorf("failed to cancel subscription with processor '%s': %w", provider, err)
					}
				}
			}
		}
	}

	// Update subscription status in database
	cancelType := models.CancelTypeUser
	subscription.Status = models.StatusCancelled
	subscription.CancelledAt = &now
	subscription.CancelType = &cancelType
	subscription.ClearRetrySchedule()
	if feedback != "" {
		subscription.CancelFeedback = &feedback
	}

	// Persist the cancellation; when a deferred (undo-window) or
	// kill-switched delete is involved, enqueue its intent IN THE SAME
	// TRANSACTION: the DeletionScheduledAt marker and the
	// nmi_delete_subscription intent commit or roll back together, so the
	// marker<->intent invariant cannot be broken by a crash between the two
	// writes. Atomic commit also kills the old relevance race (the executor
	// cannot observe the intent before the cancellation is visible).
	if (deferScheduled || scheduleKillSwitchedDelete) && s.deferDelete != nil {
		runAt := deleteAt
		if scheduleKillSwitchedDelete {
			runAt = now
		}
		if err := s.SubscriptionService.Database().TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			txdb := openrailsdb.NewWithPgxTx(tx)
			txSubSvc := NewSubscriptionService(txdb, catalog.NewPriceService(txdb), catalog.NewProductService(txdb), nil, nil, nil, s.clock)
			if err := txSubSvc.Update(ctx, subscription); err != nil {
				return fmt.Errorf("failed to update subscription status: %w", err)
			}
			return s.deferDelete.WithTx(tx).ScheduleNMIDelete(ctx, userID, subscription.ID, runAt)
		}); err != nil {
			return fmt.Errorf("failed to persist cancellation with deferred delete intent: %w", err)
		}
	} else if err := s.SubscriptionService.Update(ctx, subscription); err != nil {
		return fmt.Errorf("failed to update subscription status: %w", err)
	}

	// Add notification
	notification := &models.NotificationQueue{
		ID:              uuidutil.NewV7(),
		MerchantSubjectID: identity.MerchantSubjectIDFromString(userID).UUID(),
		EventType:       models.NotificationPremiumEnded,
		Data:            map[string]any{"reason": string(PremiumEndReasonUserCancel)},
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

func accessFromSubscription(sub *models.Subscription) *UserAccessGrant {
	grant := &UserAccessGrant{
		Kind:        "subscription",
		Entitlement: "premium",
		Processor:   string(sub.Processor),
	}
	if subID := sub.ID; subID != uuid.Nil {
		grant.SubscriptionID = &subID
	}
	if sub.ProcessorSubscriptionID != "" {
		psid := sub.ProcessorSubscriptionID
		grant.ProcessorSubscriptionID = &psid
	}
	if sub.CurrentPeriodStartsAt != nil && !sub.CurrentPeriodStartsAt.IsZero() {
		grant.StartAt = *sub.CurrentPeriodStartsAt
	} else {
		grant.StartAt = sub.StartedAt
	}
	if sub.CurrentPeriodEndsAt != nil && !sub.CurrentPeriodEndsAt.IsZero() {
		grant.EndAt = sub.CurrentPeriodEndsAt
	}
	return grant
}

func (s *UserSubscriptionService) activeEntitlementAccess(ctx context.Context, userID string) (*UserAccessGrant, error) {
	grants, err := s.entitlementAccessGrants(ctx, userID, nil)
	if err != nil {
		return nil, err
	}
	if len(grants) > 0 {
		return grants[0], nil
	}
	return nil, nil
}

func (s *UserSubscriptionService) entitlementAccessGrants(ctx context.Context, userID string, skipSubs map[uuid.UUID]struct{}) ([]*UserAccessGrant, error) {
	if s.EntitlementService == nil {
		return nil, nil
	}
	ents, err := s.EntitlementService.ListActiveRecords(ctx, userID, s.now())
	if err != nil {
		return nil, fmt.Errorf("failed to list entitlements: %w", err)
	}
	grants := make([]*UserAccessGrant, 0, len(ents))
	for _, ent := range ents {
		if ent.Entitlement == "" {
			continue
		}
		if ent.SourceType == models.EntitlementSourceSubscription && ent.SourceID != nil {
			if _, ok := skipSubs[*ent.SourceID]; ok {
				continue
			}
		}
		grant := &UserAccessGrant{
			Kind:        "entitlement",
			Entitlement: ent.Entitlement,
			StartAt:     ent.StartAt,
			EndAt:       ent.EndAt,
		}
		if ent.SourceType != "" {
			src := ent.SourceType
			grant.SourceType = &src
			if ent.SourceType == models.EntitlementSourceSubscription && ent.SourceID != nil {
				grant.SubscriptionID = ent.SourceID
			}
		}
		if ent.SourceID != nil {
			grant.SourceID = ent.SourceID
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
	notificationService NotificationStore,
	entitlementService *entitlements.EntitlementService,
	nmiClients map[string]*nmi.NMIClient,
	clocks ...clockwork.Clock,
) *UserSubscriptionService {
	return &UserSubscriptionService{
		NMIClients:          nmiClients,
		SubscriptionService: subscriptionService,
		ProductService:      productService,
		PriceService:        priceService,
		PaymentService:      paymentService,
		NotificationService: notificationService,
		EntitlementService:  entitlementService,
		clock:               timeutil.FirstClock(clocks...),
	}
}
