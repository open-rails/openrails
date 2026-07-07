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
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// RepriceService implements #773's reprice primitive: moving an existing
// subscriber to a different (same-product, same-currency, active) price at
// their next renewal on/after an effective date. Grandfathering (do nothing —
// archive the old price, keep billing existing subscribers) already exists by
// construction; this is the explicit "move them" operation, always scheduled
// (never mid-cycle), always inspectable and cancelable before it takes effect.
type RepriceService struct {
	db            *db.DB
	repo          *RepriceRepo
	prices        *catalog.PriceService
	subscriptions *SubscriptionService
	notifications *NotificationService
	clock         clockwork.Clock
}

func NewRepriceService(d *db.DB, repo *RepriceRepo, prices *catalog.PriceService, subscriptions *SubscriptionService, notifications *NotificationService, clock clockwork.Clock) *RepriceService {
	return &RepriceService{
		db:            d,
		repo:          repo,
		prices:        prices,
		subscriptions: subscriptions,
		notifications: notifications,
		clock:         timeutil.FirstClock(clock),
	}
}

func (s *RepriceService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// validateRepriceConstraints is the fail-closed constraint set (#773): to_price
// must be on the same product, same currency, and active. Cross-product moves
// are plan changes (#778, explicitly deferred); an FX-crossing or inactive
// target is refused outright.
func validateRepriceConstraints(subscriptionID uuid.UUID, from, to *models.Price) error {
	if to.Archived {
		return &RepriceConstraintError{Sentinel: ErrRepriceInactivePrice, SubscriptionID: subscriptionID, FromPriceID: from.ID, ToPriceID: to.ID}
	}
	if to.ProductID != from.ProductID {
		return &RepriceConstraintError{Sentinel: ErrRepriceCrossProduct, SubscriptionID: subscriptionID, FromPriceID: from.ID, ToPriceID: to.ID}
	}
	if !strings.EqualFold(strings.TrimSpace(to.Currency), strings.TrimSpace(from.Currency)) {
		return &RepriceConstraintError{Sentinel: ErrRepriceCrossCurrency, SubscriptionID: subscriptionID, FromPriceID: from.ID, ToPriceID: to.ID}
	}
	return nil
}

// scheduledConflict returns ErrRepriceAlreadyScheduled if the subscription
// already has a pending scheduled reprice, nil if the slot is free, or the
// underlying error on an unexpected failure.
func (s *RepriceService) scheduledConflict(ctx context.Context, subscriptionID uuid.UUID) error {
	_, err := s.repo.GetScheduledForSubscription(ctx, subscriptionID)
	if err == nil {
		return &RepriceConstraintError{Sentinel: ErrRepriceAlreadyScheduled, SubscriptionID: subscriptionID}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// Reprice schedules subscription.PriceID -> req.ToPriceID, effective at the
// subscription's first renewal on/after req.EffectiveAt (v1's only mode — no
// proration, no mid-cycle math). Emits subscription.reprice_scheduled at
// SCHEDULE time (the card-network advance-notice disclosure), not at apply
// time.
func (s *RepriceService) Reprice(ctx context.Context, req RepriceRequest) (*models.SubscriptionReprice, error) {
	sub, err := s.subscriptions.GetByID(ctx, req.SubscriptionID)
	if err != nil {
		return nil, fmt.Errorf("reprice: load subscription: %w", err)
	}
	fromPrice, err := s.prices.GetByID(ctx, sub.PriceID)
	if err != nil {
		return nil, fmt.Errorf("reprice: load current price: %w", err)
	}
	toPrice, err := s.prices.GetByID(ctx, req.ToPriceID)
	if err != nil {
		return nil, fmt.Errorf("reprice: load target price: %w", err)
	}
	if err := validateRepriceConstraints(req.SubscriptionID, fromPrice, toPrice); err != nil {
		return nil, err
	}
	if err := s.scheduledConflict(ctx, req.SubscriptionID); err != nil {
		return nil, err
	}

	rr, err := s.repo.CreateSubscriptionReprice(ctx, req.SubscriptionID, fromPrice.ID, toPrice.ID, req.EffectiveAt, nil)
	if err != nil {
		return nil, fmt.Errorf("reprice: schedule: %w", err)
	}
	s.emitScheduledNotification(ctx, sub, fromPrice, toPrice, req.EffectiveAt)
	return rr, nil
}

// RepriceAllPriorVersions bulk-schedules every ACTIVE subscription pinned to a
// PRIOR version of key (the archived members of its #774 version chain) to
// move to key's CURRENT price at req.EffectiveAt — "end the grandfather
// window" / a full price-increase rollout. Per-subscription constraint
// failures or scheduling conflicts are SKIPPED (recorded with a reason), never
// abort the whole batch.
func (s *RepriceService) RepriceAllPriorVersions(ctx context.Context, req RepriceAllPriorVersionsRequest) (*RepriceBatchResult, error) {
	key := strings.TrimSpace(req.PriceKey)
	if key == "" {
		return nil, fmt.Errorf("reprice_all_prior_versions: price_key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	toPrice, err := s.prices.GetCurrentByKey(ctx, tid.UUID(), key)
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions: price key %q has no current price: %w", key, err)
	}
	priorVersions, err := s.prices.ListPriorVersionsByKey(ctx, tid.UUID(), key)
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions: list prior versions: %w", err)
	}
	if len(priorVersions) == 0 {
		batch, err := s.repo.CreateBatch(ctx, &key, toPrice.ID, req.EffectiveAt, 0, 0, 0)
		if err != nil {
			return nil, err
		}
		return &RepriceBatchResult{BatchID: batch.ID, ToPriceID: toPrice.ID}, nil
	}
	priorByID := make(map[uuid.UUID]*models.Price, len(priorVersions))
	priorIDs := make([]uuid.UUID, 0, len(priorVersions))
	for _, p := range priorVersions {
		priorByID[p.ID] = p
		priorIDs = append(priorIDs, p.ID)
	}

	subs, err := s.repo.ListActiveSubscriptionsByPriceIDs(ctx, priorIDs)
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions: list affected subscriptions: %w", err)
	}

	type toSchedule struct {
		sub  *models.Subscription
		from *models.Price
	}
	var (
		schedule []toSchedule
		skipped  []RepriceOutcome
	)
	for _, sub := range subs {
		fromPrice := priorByID[sub.PriceID]
		if fromPrice == nil {
			// Should not happen (sub.PriceID came from priorIDs) — skip defensively.
			skipped = append(skipped, RepriceOutcome{SubscriptionID: sub.ID, Reason: "current price not found among prior versions"})
			continue
		}
		if err := validateRepriceConstraints(sub.ID, fromPrice, toPrice); err != nil {
			skipped = append(skipped, RepriceOutcome{SubscriptionID: sub.ID, Reason: err.Error()})
			continue
		}
		if err := s.scheduledConflict(ctx, sub.ID); err != nil {
			skipped = append(skipped, RepriceOutcome{SubscriptionID: sub.ID, Reason: err.Error()})
			continue
		}
		schedule = append(schedule, toSchedule{sub: sub, from: fromPrice})
	}

	batch, err := s.repo.CreateBatch(ctx, &key, toPrice.ID, req.EffectiveAt, len(subs), len(schedule), len(skipped))
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions: create batch: %w", err)
	}
	result := &RepriceBatchResult{BatchID: batch.ID, ToPriceID: toPrice.ID, Matched: len(subs), Skipped: skipped}
	batchID := batch.ID
	for _, item := range schedule {
		rr, err := s.repo.CreateSubscriptionReprice(ctx, item.sub.ID, item.from.ID, toPrice.ID, req.EffectiveAt, &batchID)
		if err != nil {
			return nil, fmt.Errorf("reprice_all_prior_versions: schedule subscription %s: %w", item.sub.ID, err)
		}
		result.Scheduled = append(result.Scheduled, RepriceOutcome{SubscriptionID: item.sub.ID, RepriceID: rr.ID})
		s.emitScheduledNotification(ctx, item.sub, item.from, toPrice, req.EffectiveAt)
	}
	return result, nil
}

func (s *RepriceService) GetByID(ctx context.Context, id uuid.UUID) (*models.SubscriptionReprice, error) {
	return s.repo.GetByID(ctx, id)
}

// List returns scheduled/applied/canceled reprices — the inspect-before-effect
// surface the #777 console wizard needs.
func (s *RepriceService) List(ctx context.Context, filter SubscriptionRepriceFilter, limit, offset int) ([]*models.SubscriptionReprice, error) {
	return s.repo.List(ctx, filter, limit, offset)
}

// Cancel cancels a scheduled reprice. Returns ErrRepriceNotScheduled if it has
// already applied, was already canceled, or does not exist — cancel-before-
// effective is enforced at the DB layer (status='scheduled' predicate), so a
// reprice that already flipped is untouched by a late cancel.
func (s *RepriceService) Cancel(ctx context.Context, id uuid.UUID) error {
	return s.repo.Cancel(ctx, id)
}

// ResolveEffectivePrice is the renewal-boundary hook every renewal path calls
// before deciding what to charge (#773's "renewal/converge jobs check for a
// due scheduled reprice at the boundary, re-pin the subscription, then
// charge"). v1's ONLY effective moment: the subscription's first renewal
// on/after the reprice's effective_at. Idempotent and safe to call more than
// once per renewal (e.g. once by the amount-deciding caller, once inside
// RenewMembership): once applied, the scheduled row is gone, so later calls
// just return the (already repinned) current price unchanged.
//
// Returns the price to charge for this renewal and whether a reprice was just
// applied.
func (s *RepriceService) ResolveEffectivePrice(ctx context.Context, subscriptionID uuid.UUID) (*models.Price, bool, error) {
	sub, err := s.subscriptions.GetByID(ctx, subscriptionID)
	if err != nil {
		return nil, false, fmt.Errorf("resolve effective price: load subscription: %w", err)
	}
	scheduled, err := s.repo.GetScheduledForSubscription(ctx, subscriptionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			price, err := s.prices.GetByID(ctx, sub.PriceID)
			return price, false, err
		}
		return nil, false, fmt.Errorf("resolve effective price: load scheduled reprice: %w", err)
	}
	if !scheduled.IsDue(s.now()) {
		price, err := s.prices.GetByID(ctx, sub.PriceID)
		return price, false, err
	}

	toPrice, err := s.prices.GetByID(ctx, scheduled.ToPriceID)
	if err != nil {
		return nil, false, fmt.Errorf("resolve effective price: load target price: %w", err)
	}
	sub.PriceID = toPrice.ID
	if err := s.subscriptions.Update(ctx, sub); err != nil {
		return nil, false, fmt.Errorf("resolve effective price: re-pin subscription: %w", err)
	}
	if err := s.repo.Apply(ctx, scheduled.ID); err != nil && !errors.Is(err, ErrRepriceNotScheduled) {
		return nil, false, fmt.Errorf("resolve effective price: mark applied: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id": subscriptionID,
		"reprice_id":      scheduled.ID,
		"from_price_id":   scheduled.FromPriceID,
		"to_price_id":     toPrice.ID,
	}).Info("applied scheduled reprice at renewal boundary")
	return toPrice, true, nil
}

func (s *RepriceService) emitScheduledNotification(ctx context.Context, sub *models.Subscription, from, to *models.Price, effectiveAt time.Time) {
	if s.notifications == nil {
		return
	}
	n := &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: sub.CustomerID,
		EventType:  models.NotificationSubscriptionRepriceScheduled,
		Data: map[string]any{
			"subscription_id": sub.ID.String(),
			"from_price_id":   from.ID.String(),
			"to_price_id":     to.ID.String(),
			"old_amount":      from.Amount,
			"new_amount":      to.Amount,
			"currency":        to.Currency,
			"effective_at":    effectiveAt.UTC().Format(time.RFC3339),
		},
	}
	if err := s.notifications.CreateAndDeliver(ctx, n); err != nil {
		log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).Warn("failed to emit subscription_reprice_scheduled notification")
	}
}
