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
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
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
	config        *merchantconfig.Store
	clock         clockwork.Clock
}

func NewRepriceService(d *db.DB, repo *RepriceRepo, prices *catalog.PriceService, subscriptions *SubscriptionService, notifications *NotificationService, config *merchantconfig.Store, clock clockwork.Clock) *RepriceService {
	return &RepriceService{
		db:            d,
		repo:          repo,
		prices:        prices,
		subscriptions: subscriptions,
		notifications: notifications,
		config:        config,
		clock:         timeutil.FirstClock(clock),
	}
}

// products loads a product through a catalog service bound to the same DB
// handle (#813: cross-product cutover needs the target product's specs).
func (s *RepriceService) products(ctx context.Context, id uuid.UUID) (*models.Product, error) {
	return catalog.NewProductService(s.db).GetByID(ctx, id)
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

// noticeWindowDays (#781) resolves the merchant's configured minimum notice
// window for a price-increase reprice, falling back to
// DefaultRepriceNoticeWindowDays when the merchant has no override (or no
// config store is wired at all, e.g. some lightweight test fixtures).
func (s *RepriceService) noticeWindowDays(ctx context.Context) (int, error) {
	if s.config == nil {
		return DefaultRepriceNoticeWindowDays, nil
	}
	cfg, _, err := s.config.Get(ctx)
	if err != nil {
		return 0, fmt.Errorf("reprice: load merchant notice-window config: %w", err)
	}
	if cfg.RepriceNoticeWindowDays != nil {
		return *cfg.RepriceNoticeWindowDays, nil
	}
	return DefaultRepriceNoticeWindowDays, nil
}

// checkNoticeWindow (#781) is the fail-closed notice-window gate: an INCREASE
// (to.Amount > from.Amount) whose effectiveAt is nearer than the merchant's
// configured window violates. Decreases never violate — card-network/
// consumer-protection advance-notice requirements apply to increases only.
func (s *RepriceService) checkNoticeWindow(ctx context.Context, from, to *models.Price, effectiveAt time.Time) (violates bool, err error) {
	if to.Amount <= from.Amount {
		return false, nil
	}
	days, err := s.noticeWindowDays(ctx)
	if err != nil {
		return false, err
	}
	if days <= 0 {
		return false, nil
	}
	minEffective := s.now().Add(time.Duration(days) * 24 * time.Hour)
	return effectiveAt.Before(minEffective), nil
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
	violatesNotice, err := s.checkNoticeWindow(ctx, fromPrice, toPrice, req.EffectiveAt)
	if err != nil {
		return nil, err
	}
	if violatesNotice && !req.AcknowledgeShortNotice {
		return nil, &RepriceConstraintError{Sentinel: ErrRepriceNoticeWindowViolation, SubscriptionID: req.SubscriptionID, FromPriceID: fromPrice.ID, ToPriceID: toPrice.ID}
	}
	acknowledgedShortNotice := violatesNotice && req.AcknowledgeShortNotice
	if acknowledgedShortNotice {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": req.SubscriptionID,
			"from_price_id":   fromPrice.ID,
			"to_price_id":     toPrice.ID,
			"effective_at":    req.EffectiveAt,
		}).Warn("reprice: short-notice override acknowledged for a price increase inside the merchant's notice window")
	}

	rr, err := s.repo.CreateSubscriptionReprice(ctx, req.SubscriptionID, fromPrice.ID, toPrice.ID, req.EffectiveAt, nil, acknowledgedShortNotice)
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
		sub                     *models.Subscription
		from                    *models.Price
		acknowledgedShortNotice bool
	}
	var (
		schedule          []toSchedule
		skipped           []RepriceOutcome
		acknowledgedCount int
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
		// #781: per-subscription, since a bulk call's prior versions can carry
		// different amounts (some higher, some lower than the target) — the
		// notice window only ever gates the subset that are true increases.
		violatesNotice, err := s.checkNoticeWindow(ctx, fromPrice, toPrice, req.EffectiveAt)
		if err != nil {
			return nil, fmt.Errorf("reprice_all_prior_versions: check notice window for subscription %s: %w", sub.ID, err)
		}
		if violatesNotice && !req.AcknowledgeShortNotice {
			skipped = append(skipped, RepriceOutcome{
				SubscriptionID: sub.ID,
				Reason:         (&RepriceConstraintError{Sentinel: ErrRepriceNoticeWindowViolation, SubscriptionID: sub.ID, FromPriceID: fromPrice.ID, ToPriceID: toPrice.ID}).Error(),
			})
			continue
		}
		acknowledged := violatesNotice && req.AcknowledgeShortNotice
		if acknowledged {
			acknowledgedCount++
		}
		schedule = append(schedule, toSchedule{sub: sub, from: fromPrice, acknowledgedShortNotice: acknowledged})
	}

	batch, err := s.repo.CreateBatch(ctx, &key, toPrice.ID, req.EffectiveAt, len(subs), len(schedule), len(skipped))
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions: create batch: %w", err)
	}
	if acknowledgedCount > 0 {
		log.WithContext(ctx).WithFields(log.Fields{
			"batch_id":           batch.ID,
			"price_key":          key,
			"to_price_id":        toPrice.ID,
			"effective_at":       req.EffectiveAt,
			"acknowledged_count": acknowledgedCount,
		}).Warn("reprice_all_prior_versions: short-notice override acknowledged for a price increase inside the merchant's notice window")
	}
	result := &RepriceBatchResult{BatchID: batch.ID, ToPriceID: toPrice.ID, Matched: len(subs), Skipped: skipped}
	batchID := batch.ID
	for _, item := range schedule {
		rr, err := s.repo.CreateSubscriptionReprice(ctx, item.sub.ID, item.from.ID, toPrice.ID, req.EffectiveAt, &batchID, item.acknowledgedShortNotice)
		if err != nil {
			return nil, fmt.Errorf("reprice_all_prior_versions: schedule subscription %s: %w", item.sub.ID, err)
		}
		result.Scheduled = append(result.Scheduled, RepriceOutcome{SubscriptionID: item.sub.ID, RepriceID: rr.ID, AcknowledgedShortNotice: item.acknowledgedShortNotice})
		s.emitScheduledNotification(ctx, item.sub, item.from, toPrice, req.EffectiveAt)
	}
	return result, nil
}

// PreviewAllPriorVersions is #777's read-only dry-run counterpart to
// RepriceAllPriorVersions: same target-set resolution, but NEVER creates a
// batch, schedules a reprice, or emits a notification. Used by the console
// wizard's Step 2 affected-count preview, called BEFORE the price-edit step
// creates the new version — so unlike the real bulk call (which targets the
// key's ARCHIVED prior versions once the new one is current), this counts the
// key's WHOLE chain (current + archived): every active subscriber on it today
// is a "prior version" candidate the instant the pending edit lands.
func (s *RepriceService) PreviewAllPriorVersions(ctx context.Context, priceKey string) (*RepricePreviewResult, error) {
	key := strings.TrimSpace(priceKey)
	if key == "" {
		return nil, fmt.Errorf("reprice_all_prior_versions preview: price_key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	toPrice, err := s.prices.GetCurrentByKey(ctx, tid.UUID(), key)
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions preview: price key %q has no current price: %w", key, err)
	}
	chain, err := s.prices.ListChainByKey(ctx, tid.UUID(), key)
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions preview: list version chain: %w", err)
	}
	if len(chain) == 0 {
		return &RepricePreviewResult{PriceKey: key, ToPriceID: toPrice.ID}, nil
	}
	chainIDs := make([]uuid.UUID, 0, len(chain))
	for _, p := range chain {
		chainIDs = append(chainIDs, p.ID)
	}
	subs, err := s.repo.ListActiveSubscriptionsByPriceIDs(ctx, chainIDs)
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions preview: count affected subscriptions: %w", err)
	}
	return &RepricePreviewResult{PriceKey: key, ToPriceID: toPrice.ID, Matched: len(subs)}, nil
}

// ListBatchesForKey lists a price key's bulk reprice operations, most recent
// first — the #777 console price page's "is there a pending migration"
// surface.
func (s *RepriceService) ListBatchesForKey(ctx context.Context, priceKey string, limit, offset int) ([]*models.RepriceBatch, error) {
	key := strings.TrimSpace(priceKey)
	if key == "" {
		return nil, fmt.Errorf("list reprice batches: price_key required")
	}
	return s.repo.ListBatchesByPriceKey(ctx, key, limit, offset)
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
	if toPrice.ProductID != sub.ProductID {
		// #813 plan_change: cross-product cutover — move the product ref and
		// cut entitlement/credit snapshots over with the price. The renewal
		// grant that follows re-derives windows from the new snapshots.
		newProduct, err := s.products(ctx, toPrice.ProductID)
		if err != nil {
			return nil, false, fmt.Errorf("resolve effective price: load target product: %w", err)
		}
		sub.ProductID = toPrice.ProductID
		sub.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(newProduct.EntitlementsSpec)
		sub.CreditsSpecSnapshot = models.CloneCreditsSpec(newProduct.CreditsSpec)
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
