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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
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

// RepriceAllPriorVersions bulk-schedules every ACTIVE subscription pinned to a
// PRIOR version of key (the archived members of its #774 version chain) to
// move to key's CURRENT price at req.EffectiveAt — "end the grandfather
// window" / a full price-increase rollout. Per-subscription constraint
// failures or scheduling conflicts are SKIPPED (recorded with a reason), never
// abort the whole batch.
func (s *RepriceService) CreateBatch(ctx context.Context, req billing.CreateRepriceBatchParams) (*billing.RepriceBatchResult, error) {
	key := strings.TrimSpace(req.PriceKey)
	if key == "" || strings.TrimSpace(req.ProductKey) == "" {
		return nil, apperr.Invalidf("reprice_all_prior_versions: product_key and price_key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	toPrice, err := s.prices.GetCurrentByProductKey(ctx, tid.UUID(), req.ProductKey, key)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, fmt.Errorf("%w: price key %q has no current price", ErrRepricePriceKeyNotFound, key)
		}
		return nil, fmt.Errorf("reprice_all_prior_versions: load current price for key %q: %w", key, err)
	}
	priorVersions, err := s.prices.ListPriorVersionsByKey(ctx, tid.UUID(), toPrice.ProductID, key)
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions: list prior versions: %w", err)
	}
	if len(priorVersions) == 0 {
		batchID, err := s.repo.CreateBatch(ctx, &key, toPrice.ID, req.EffectiveAt, 0, 0)
		if err != nil {
			return nil, err
		}
		return &billing.RepriceBatchResult{BatchID: billing.RepriceBatchID(batchID), ToPriceID: billing.PriceID(toPrice.ID), Scheduled: []billing.RepriceOutcome{}, Skipped: []billing.RepriceOutcome{}}, nil
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
		skipped           = []billing.RepriceOutcome{}
		acknowledgedCount int
	)
	for _, sub := range subs {
		fromPrice := priorByID[sub.PriceID]
		if fromPrice == nil {
			// Should not happen (sub.PriceID came from priorIDs) — skip defensively.
			skipped = append(skipped, billing.RepriceOutcome{SubscriptionID: billing.SubscriptionID(sub.ID), Reason: ref("current price not found among prior versions")})
			continue
		}
		if err := validateRepriceConstraints(sub.ID, fromPrice, toPrice); err != nil {
			skipped = append(skipped, billing.RepriceOutcome{SubscriptionID: billing.SubscriptionID(sub.ID), Reason: ref(err.Error())})
			continue
		}
		if err := s.scheduledConflict(ctx, sub.ID); err != nil {
			skipped = append(skipped, billing.RepriceOutcome{SubscriptionID: billing.SubscriptionID(sub.ID), Reason: ref(err.Error())})
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
			skipped = append(skipped, billing.RepriceOutcome{
				SubscriptionID: billing.SubscriptionID(sub.ID),
				Reason:         ref((&RepriceConstraintError{Sentinel: ErrRepriceNoticeWindowViolation, SubscriptionID: sub.ID, FromPriceID: fromPrice.ID, ToPriceID: toPrice.ID}).Error()),
			})
			continue
		}
		acknowledged := violatesNotice && req.AcknowledgeShortNotice
		if acknowledged {
			acknowledgedCount++
		}
		schedule = append(schedule, toSchedule{sub: sub, from: fromPrice, acknowledgedShortNotice: acknowledged})
	}

	batchID, err := s.repo.CreateBatch(ctx, &key, toPrice.ID, req.EffectiveAt, len(subs), len(skipped))
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions: create batch: %w", err)
	}
	if acknowledgedCount > 0 {
		log.WithContext(ctx).WithFields(log.Fields{
			"batch_id":           batchID,
			"price_key":          key,
			"to_price_id":        toPrice.ID,
			"effective_at":       req.EffectiveAt,
			"acknowledged_count": acknowledgedCount,
		}).Warn("reprice_all_prior_versions: short-notice override acknowledged for a price increase inside the merchant's notice window")
	}
	result := &billing.RepriceBatchResult{BatchID: billing.RepriceBatchID(batchID), ToPriceID: billing.PriceID(toPrice.ID), Matched: len(subs), Scheduled: []billing.RepriceOutcome{}, Skipped: skipped}
	for _, item := range schedule {
		rr, err := s.repo.CreateSubscriptionReprice(ctx, item.sub.ID, item.from.ID, toPrice.ID, req.EffectiveAt, &batchID, item.acknowledgedShortNotice)
		if err != nil {
			return nil, fmt.Errorf("reprice_all_prior_versions: schedule subscription %s: %w", item.sub.ID, err)
		}
		result.Scheduled = append(result.Scheduled, billing.RepriceOutcome{SubscriptionID: billing.SubscriptionID(item.sub.ID), RepriceID: ref(billing.RepriceID(rr.ID)), AcknowledgedShortNotice: item.acknowledgedShortNotice})
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
func (s *RepriceService) PreviewBatch(ctx context.Context, productKey, priceKey string) (*billing.RepriceBatchPreview, error) {
	key := strings.TrimSpace(priceKey)
	if key == "" || strings.TrimSpace(productKey) == "" {
		return nil, apperr.Invalidf("reprice_all_prior_versions preview: product_key and price_key required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	toPrice, err := s.prices.GetCurrentByProductKey(ctx, tid.UUID(), productKey, key)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, fmt.Errorf("%w: price key %q has no current price", ErrRepricePriceKeyNotFound, key)
		}
		return nil, fmt.Errorf("reprice_all_prior_versions preview: load current price for key %q: %w", key, err)
	}
	chain, err := s.prices.ListChainByKey(ctx, tid.UUID(), toPrice.ProductID, key)
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions preview: list version chain: %w", err)
	}
	if len(chain) == 0 {
		return &billing.RepriceBatchPreview{ProductKey: productKey, PriceKey: key, ToPriceID: billing.PriceID(toPrice.ID)}, nil
	}
	chainIDs := make([]uuid.UUID, 0, len(chain))
	for _, p := range chain {
		chainIDs = append(chainIDs, p.ID)
	}
	subs, err := s.repo.ListActiveSubscriptionsByPriceIDs(ctx, chainIDs)
	if err != nil {
		return nil, fmt.Errorf("reprice_all_prior_versions preview: count affected subscriptions: %w", err)
	}
	return &billing.RepriceBatchPreview{ProductKey: productKey, PriceKey: key, ToPriceID: billing.PriceID(toPrice.ID), Matched: len(subs)}, nil
}

// GetBatch reads one batch with its reprices counted by status.
func (s *RepriceService) GetBatch(ctx context.Context, id billing.RepriceBatchID) (billing.RepriceBatch, error) {
	return s.repo.GetBatch(ctx, id.UUID())
}

// ListBatches is one page of the merchant's batches, newest first.
func (s *RepriceService) ListBatches(ctx context.Context, params billing.RepriceBatchListParams) (billing.ListPage[billing.RepriceBatch], error) {
	if params.IDs != nil {
		rows, err := s.repo.ListBatchesByIDs(ctx, uuidutil.Of(params.IDs))
		return billing.ListPage[billing.RepriceBatch]{Items: rows}, err
	}
	if params.PriceKey != "" && strings.TrimSpace(params.ProductKey) == "" {
		return billing.ListPage[billing.RepriceBatch]{}, apperr.Invalidf("product_key is required with price_key")
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.RepriceBatch]{}, err
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return billing.ListPage[billing.RepriceBatch]{}, err
	}
	var key *string
	if k := strings.TrimSpace(params.PriceKey); k != "" {
		key = &k
	}
	rows, err := s.repo.ListBatches(ctx, params.ProductKey, key, pagination.Fetch(limit), afterAt, afterID)
	if err != nil {
		return billing.ListPage[billing.RepriceBatch]{}, err
	}
	return pagination.Cut(rows, limit, func(b billing.RepriceBatch) any {
		return pagination.TimeID{At: b.CreatedAt, ID: b.ID.UUID()}
	}), nil
}

// GetReprice reads one reprice.
func (s *RepriceService) GetReprice(ctx context.Context, id billing.RepriceID) (billing.Reprice, error) {
	out, err := s.repo.GetByID(ctx, id.UUID())
	if db.IsNotFound(err) {
		return billing.Reprice{}, ErrRepriceNotFound
	}
	if err != nil {
		return billing.Reprice{}, err
	}
	return Reprice(out), nil
}

// ListReprices is one page of the merchant's reprices, newest first.
func (s *RepriceService) ListReprices(ctx context.Context, params billing.RepriceListParams) (billing.ListPage[billing.Reprice], error) {
	if params.IDs != nil {
		rows, err := s.repo.ListByIDs(ctx, uuidutil.Of(params.IDs))
		return pagination.Map(billing.ListPage[*models.SubscriptionReprice]{Items: rows}, Reprice), err
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return billing.ListPage[billing.Reprice]{}, err
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return billing.ListPage[billing.Reprice]{}, err
	}
	var f RepriceFilter
	if !params.SubscriptionID.IsZero() {
		f.SubscriptionID = ref(params.SubscriptionID.UUID())
	}
	if !params.RepriceBatchID.IsZero() {
		f.RepriceBatchID = ref(params.RepriceBatchID.UUID())
	}
	if params.Status != "" {
		f.Status = &params.Status
	}
	rows, err := s.repo.ListPage(ctx, f, pagination.Fetch(limit), afterAt, afterID)
	if err != nil {
		return billing.ListPage[billing.Reprice]{}, err
	}
	page := pagination.Cut(rows, limit, func(r *models.SubscriptionReprice) any {
		return pagination.TimeID{At: r.CreatedAt, ID: r.ID}
	})
	return pagination.Map(page, Reprice), nil
}

// CancelReprice cancels a scheduled reprice and returns it. A reprice that
// already applied, was canceled or is blocked is ErrRepriceNotScheduled: the
// status predicate leaves a reprice that already flipped untouched.
func (s *RepriceService) CancelReprice(ctx context.Context, id billing.RepriceID) (billing.Reprice, error) {
	if _, err := s.GetReprice(ctx, id); err != nil {
		return billing.Reprice{}, err
	}
	if err := s.repo.Cancel(ctx, id.UUID()); err != nil {
		return billing.Reprice{}, err
	}
	return s.GetReprice(ctx, id)
}

func (s *RepriceService) emitScheduledNotification(ctx context.Context, sub *models.Subscription, from, to *models.Price, effectiveAt time.Time) {
	if s.notifications == nil {
		return
	}
	n := &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: sub.CustomerID,
		EventType:  models.NotificationSubscriptionRepriceScheduled,
		Data: billing.NotificationData{
			SubscriptionID: billing.SubscriptionID(sub.ID),
			FromPriceID:    billing.PriceID(from.ID),
			ToPriceID:      billing.PriceID(to.ID),
			OldAmount:      &from.Amount,
			NewAmount:      &to.Amount,
			Currency:       to.Currency,
			EffectiveAt:    ptrTime(effectiveAt.UTC()),
		},
	}
	if err := s.notifications.CreateAndDeliver(ctx, n); err != nil {
		log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).Warn("failed to emit subscription_reprice_scheduled notification")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
