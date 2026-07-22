package subscriptions

import (
	"context"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

// RepriceRepo is the persistence layer for #773's reprice primitive:
// reprice_batches (the header row for one bulk/ad-hoc operation) and
// subscription_reprices (one scheduled/applied/canceled row per affected
// subscription).
type RepriceRepo struct {
	db *db.DB
}

func NewRepriceRepo(d *db.DB) *RepriceRepo { return &RepriceRepo{db: d} }

// CreateBatch records a bulk (or single ad-hoc) reprice operation's header.
func (r *RepriceRepo) CreateBatch(ctx context.Context, priceKey *string, toPriceID uuid.UUID, effectiveAt time.Time, matched, scheduled, skipped int) (*models.RepriceBatch, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	matched32, _ := safecast.Convert[int32](matched)
	scheduled32, _ := safecast.Convert[int32](scheduled)
	skipped32, _ := safecast.Convert[int32](skipped)
	row, err := r.db.Gen(ctx).CreateRepriceBatch(ctx, gen.CreateRepriceBatchParams{
		MerchantID:             tid.UUID(),
		PriceKey:               priceKey,
		ToPriceID:              toPriceID,
		EffectiveAt:            effectiveAt,
		SubscriptionsMatched:   matched32,
		SubscriptionsScheduled: scheduled32,
		SubscriptionsSkipped:   skipped32,
	})
	if err != nil {
		return nil, err
	}
	return models.RepriceBatchFromGen(row), nil
}

func (r *RepriceRepo) GetBatchByID(ctx context.Context, id uuid.UUID) (*models.RepriceBatch, error) {
	row, err := r.db.Gen(ctx).GetRepriceBatchByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return models.RepriceBatchFromGen(row), nil
}

// CreateSubscriptionReprice schedules one subscription's price move.
// batchID is nil for a single ad-hoc reprice() call. acknowledgedShortNotice
// (#781) records whether this row's effective_at was inside the merchant's
// configured notice window and was scheduled anyway via an explicit override.
func (r *RepriceRepo) CreateSubscriptionReprice(ctx context.Context, subscriptionID, fromPriceID, toPriceID uuid.UUID, effectiveAt time.Time, batchID *uuid.UUID, acknowledgedShortNotice bool) (*models.SubscriptionReprice, error) {
	return r.createReprice(ctx, subscriptionID, fromPriceID, toPriceID, effectiveAt, batchID, acknowledgedShortNotice, models.RepriceKindReprice)
}

// CreatePlanChangeReprice (#813) schedules one subscription's cross-product
// plan migration row (kind=plan_change).
func (r *RepriceRepo) CreatePlanChangeReprice(ctx context.Context, subscriptionID, fromPriceID, toPriceID uuid.UUID, effectiveAt time.Time, batchID *uuid.UUID, acknowledgedShortNotice bool) (*models.SubscriptionReprice, error) {
	return r.createReprice(ctx, subscriptionID, fromPriceID, toPriceID, effectiveAt, batchID, acknowledgedShortNotice, models.RepriceKindPlanChange)
}

func (r *RepriceRepo) createReprice(ctx context.Context, subscriptionID, fromPriceID, toPriceID uuid.UUID, effectiveAt time.Time, batchID *uuid.UUID, acknowledgedShortNotice bool, kind models.RepriceKind) (*models.SubscriptionReprice, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).CreateSubscriptionReprice(ctx, gen.CreateSubscriptionRepriceParams{
		MerchantID:              tid.UUID(),
		SubscriptionID:          subscriptionID,
		FromPriceID:             fromPriceID,
		ToPriceID:               toPriceID,
		EffectiveAt:             effectiveAt,
		RepriceBatchID:          batchID,
		AcknowledgedShortNotice: acknowledgedShortNotice,
		Kind:                    string(kind),
	})
	if err != nil {
		return nil, err
	}
	return models.SubscriptionRepriceFromGen(row), nil
}

// CreateBlockedReprice (#813) records a plan-migration cohort member the
// engine could NOT auto-schedule — the per-subscription ledger row that keeps
// batch totals complete. Terminal at insert.
func (r *RepriceRepo) CreateBlockedReprice(ctx context.Context, subscriptionID, fromPriceID, toPriceID uuid.UUID, effectiveAt time.Time, batchID *uuid.UUID, kind models.RepriceKind, reason string) (*models.SubscriptionReprice, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).CreateBlockedSubscriptionReprice(ctx, gen.CreateBlockedSubscriptionRepriceParams{
		MerchantID:     tid.UUID(),
		SubscriptionID: subscriptionID,
		FromPriceID:    fromPriceID,
		ToPriceID:      toPriceID,
		EffectiveAt:    effectiveAt,
		RepriceBatchID: batchID,
		Kind:           string(kind),
		BlockedReason:  reason,
	})
	if err != nil {
		return nil, err
	}
	return models.SubscriptionRepriceFromGen(row), nil
}

// BlockScheduledReprice (#813) transitions a scheduled row to blocked (rail
// push failed after creation). Idempotent via the status predicate.
func (r *RepriceRepo) BlockScheduledReprice(ctx context.Context, id uuid.UUID, reason string) error {
	_, err := r.db.Gen(ctx).BlockSubscriptionReprice(ctx, gen.BlockSubscriptionRepriceParams{ID: id, BlockedReason: reason})
	return err
}

// CreatePlanMigrationBatch (#813) records a plan-migration operation's header.
func (r *RepriceRepo) CreatePlanMigrationBatch(ctx context.Context, sourcePriceID, toPriceID uuid.UUID, effectiveAt time.Time, fallbackPolicy string, matched, scheduled, skipped, blocked int) (*models.RepriceBatch, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	matched32, _ := safecast.Convert[int32](matched)
	scheduled32, _ := safecast.Convert[int32](scheduled)
	skipped32, _ := safecast.Convert[int32](skipped)
	blocked32, _ := safecast.Convert[int32](blocked)
	row, err := r.db.Gen(ctx).CreatePlanMigrationBatch(ctx, gen.CreatePlanMigrationBatchParams{
		MerchantID:             tid.UUID(),
		ToPriceID:              toPriceID,
		EffectiveAt:            effectiveAt,
		SourcePriceID:          sourcePriceID,
		FallbackPolicy:         fallbackPolicy,
		SubscriptionsMatched:   matched32,
		SubscriptionsScheduled: scheduled32,
		SubscriptionsSkipped:   skipped32,
		SubscriptionsBlocked:   blocked32,
	})
	if err != nil {
		return nil, err
	}
	return models.RepriceBatchFromGen(row), nil
}

// UpdatePlanMigrationBatchCounts (#813) re-syncs a batch header's
// scheduled/blocked counts after rail pushes degrade rows — the header must
// always agree with its per-subscription rows.
func (r *RepriceRepo) UpdatePlanMigrationBatchCounts(ctx context.Context, id uuid.UUID, scheduled, blocked int) error {
	scheduled32, _ := safecast.Convert[int32](scheduled)
	blocked32, _ := safecast.Convert[int32](blocked)
	_, err := r.db.Gen(ctx).UpdatePlanMigrationBatchCounts(ctx, gen.UpdatePlanMigrationBatchCountsParams{
		ID:                     id,
		SubscriptionsScheduled: scheduled32,
		SubscriptionsBlocked:   blocked32,
	})
	return err
}

// ListMigratableSubscriptionsByPriceID (#813) returns the plan-migration
// cohort: every subscription still billing (or being dunned) on the price.
func (r *RepriceRepo) ListMigratableSubscriptionsByPriceID(ctx context.Context, priceID uuid.UUID) ([]*models.Subscription, error) {
	rows, err := r.db.Gen(ctx).ListMigratableSubscriptionsByPriceID(ctx, priceID)
	if err != nil {
		return nil, err
	}
	out := make([]*models.Subscription, 0, len(rows))
	for _, row := range rows {
		sub, err := models.SubscriptionFromGen(row)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, nil
}

func (r *RepriceRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.SubscriptionReprice, error) {
	row, err := r.db.Gen(ctx).GetSubscriptionRepriceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return models.SubscriptionRepriceFromGen(row), nil
}

// GetScheduledForSubscription returns the subscription's current scheduled
// reprice, or pgx.ErrNoRows if none exists. At most one can exist at a time
// (uq_subscription_reprices_one_scheduled).
func (r *RepriceRepo) GetScheduledForSubscription(ctx context.Context, subscriptionID uuid.UUID) (*models.SubscriptionReprice, error) {
	row, err := r.db.Gen(ctx).GetScheduledRepriceForSubscription(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}
	return models.SubscriptionRepriceFromGen(row), nil
}

type SubscriptionRepriceFilter struct {
	SubscriptionID *uuid.UUID
	RepriceBatchID *uuid.UUID
	Status         *models.RepriceStatus
}

func (r *RepriceRepo) List(ctx context.Context, filter SubscriptionRepriceFilter, limit, offset int) ([]*models.SubscriptionReprice, error) {
	var status *string
	if filter.Status != nil {
		s := string(*filter.Status)
		status = &s
	}
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := r.db.Gen(ctx).ListSubscriptionReprices(ctx, gen.ListSubscriptionRepricesParams{
		SubscriptionID: filter.SubscriptionID,
		RepriceBatchID: filter.RepriceBatchID,
		Status:         status,
		PageLimit:      limit32,
		PageOffset:     offset32,
	})
	if err != nil {
		return nil, err
	}
	return models.SubscriptionRepricesFromGen(rows), nil
}

// Cancel cancels a scheduled reprice. Returns ErrRepriceNotScheduled if the
// row is not (or is no longer) in status=scheduled — the cancel-before-
// effective contract.
func (r *RepriceRepo) Cancel(ctx context.Context, id uuid.UUID) error {
	rows, err := r.db.Gen(ctx).CancelSubscriptionReprice(ctx, id)
	if err != nil {
		return err
	}
	if rows < 1 {
		return ErrRepriceNotScheduled
	}
	return nil
}

// Apply marks a scheduled reprice as applied. Returns ErrRepriceNotScheduled
// if it was already applied/canceled by a concurrent caller — the renewal
// boundary pickup is safe to retry.
func (r *RepriceRepo) Apply(ctx context.Context, id uuid.UUID) error {
	rows, err := r.db.Gen(ctx).ApplySubscriptionReprice(ctx, id)
	if err != nil {
		return err
	}
	if rows < 1 {
		return ErrRepriceNotScheduled
	}
	return nil
}

// ListBatchesByPriceKey lists a key's bulk reprice operations, most recent
// first (#777: the console's price page needs "is there a pending migration
// for this price key" without already knowing a batch id).
func (r *RepriceRepo) ListBatchesByPriceKey(ctx context.Context, priceKey string, limit, offset int) ([]*models.RepriceBatch, error) {
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := r.db.Gen(ctx).ListRepriceBatchesByPriceKey(ctx, gen.ListRepriceBatchesByPriceKeyParams{
		PriceKey:   priceKey,
		PageLimit:  limit32,
		PageOffset: offset32,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*models.RepriceBatch, 0, len(rows))
	for _, row := range rows {
		out = append(out, models.RepriceBatchFromGen(row))
	}
	return out, nil
}

// ListActiveSubscriptionsByPriceIDs returns every ACTIVE subscription pinned
// to one of the given price rows — reprice_all_prior_versions' match set.
func (r *RepriceRepo) ListActiveSubscriptionsByPriceIDs(ctx context.Context, priceIDs []uuid.UUID) ([]*models.Subscription, error) {
	rows, err := r.db.Gen(ctx).ListActiveSubscriptionsByPriceIDs(ctx, priceIDs)
	if err != nil {
		return nil, err
	}
	out := make([]*models.Subscription, 0, len(rows))
	for _, row := range rows {
		s, err := models.SubscriptionFromGen(row)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
