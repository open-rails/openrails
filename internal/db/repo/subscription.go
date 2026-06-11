package repo

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/query"
)

type SubscriptionFilters struct {
	UserID          string
	Status          string
	PriceID         uuid.UUID
	Processor       string
	CreatedAfter    *time.Time
	CreatedBefore   *time.Time
	CancelledAfter  *time.Time
	CancelledBefore *time.Time
	ExpiresBefore   *time.Time
	SortBy          string // created_at (default), expires_at, cancelled_at
	SortOrder       string // asc, desc (default)
}

type SubscriptionRepo struct {
	db *db.DB
}

func NewSubscriptionRepo(d *db.DB) *SubscriptionRepo { return &SubscriptionRepo{db: d} }

func subscriptionInsertParams(s *models.Subscription) (gen.CreateSubscriptionParams, error) {
	entSnap, err := toJSONB(s.EntitlementsSpecSnapshot)
	if err != nil {
		return gen.CreateSubscriptionParams{}, err
	}
	credSnap, err := toJSONB(s.CreditsSpecSnapshot)
	if err != nil {
		return gen.CreateSubscriptionParams{}, err
	}
	var priceID *uuid.UUID
	if s.PriceID != uuid.Nil {
		priceID = &s.PriceID
	}
	var cancelType *string
	if s.CancelType != nil {
		ct := string(*s.CancelType)
		cancelType = &ct
	}
	return gen.CreateSubscriptionParams{
		ID:                       s.ID,
		TenantSubjectID:          s.TenantSubjectID,
		ProductID:                s.ProductID,
		PriceID:                  priceID,
		ScheduledPriceID:         s.ScheduledPriceID,
		EntitlementsSpecSnapshot: entSnap,
		CreditsSpecSnapshot:      credSnap,
		Status:                   string(s.Status),
		StartedAt:                s.StartedAt,
		EndedAt:                  s.EndedAt,
		CurrentPeriodStartsAt:    s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:      s.CurrentPeriodEndsAt,
		Processor:                string(s.Processor),
		ProcessorSubscriptionID:  s.ProcessorSubscriptionID,
		UserEmail:                s.UserEmail,
		PaymentMethodID:          s.PaymentMethodID,
		LastRetryAt:              s.LastRetryAt,
		RetryAttempts:            intPtrTo32(s.RetryAttempts),
		NextRetryAt:              s.NextRetryAt,
		GraceEndsAt:              s.GraceEndsAt,
		CancelFeedback:           s.CancelFeedback,
		CancelType:               cancelType,
		CancelledAt:              s.CancelledAt,
		DeletionScheduledAt:      s.DeletionScheduledAt,
		GatewayResponse:          s.Metadata,
		CreatedAt:                s.CreatedAt,
		UpdatedAt:                s.UpdatedAt,
	}, nil
}

func (r *SubscriptionRepo) Create(ctx context.Context, s *models.Subscription) error {
	if err := ensureTenantSubjectRow(ctx, r.db.Qx(ctx), uuid.Nil, s.TenantSubjectID); err != nil {
		return err
	}
	params, err := subscriptionInsertParams(s)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).CreateSubscription(ctx, params)
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *SubscriptionRepo) Update(ctx context.Context, s *models.Subscription) error {
	return r.UpdateAt(ctx, s, time.Now())
}

func (r *SubscriptionRepo) UpdateAt(ctx context.Context, s *models.Subscription, now time.Time) error {
	// All columns are written explicitly so nil values CLEAR fields
	// (CancelledAt, EndedAt, ...) when reactivating subscriptions.
	if now.IsZero() {
		now = time.Now()
	}
	s.UpdatedAt = now

	entSnap, err := toJSONB(s.EntitlementsSpecSnapshot)
	if err != nil {
		return err
	}
	credSnap, err := toJSONB(s.CreditsSpecSnapshot)
	if err != nil {
		return err
	}
	var priceID *uuid.UUID
	if s.PriceID != uuid.Nil {
		priceID = &s.PriceID
	}
	var cancelType *string
	if s.CancelType != nil {
		ct := string(*s.CancelType)
		cancelType = &ct
	}
	rows, err := r.db.Gen(ctx).UpdateSubscriptionAt(ctx, gen.UpdateSubscriptionAtParams{
		ID:                       s.ID,
		PriceID:                  priceID,
		ProductID:                s.ProductID,
		EntitlementsSpecSnapshot: entSnap,
		CreditsSpecSnapshot:      credSnap,
		Status:                   gen.BillingSubscriptionStatus(s.Status),
		StartedAt:                s.StartedAt,
		EndedAt:                  s.EndedAt,
		CurrentPeriodStartsAt:    s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:      s.CurrentPeriodEndsAt,
		Processor:                string(s.Processor),
		ProcessorSubscriptionID:  s.ProcessorSubscriptionID,
		UserEmail:                s.UserEmail,
		PaymentMethodID:          s.PaymentMethodID,
		LastRetryAt:              s.LastRetryAt,
		RetryAttempts:            intPtrTo32(s.RetryAttempts),
		NextRetryAt:              s.NextRetryAt,
		GraceEndsAt:              s.GraceEndsAt,
		CancelFeedback:           s.CancelFeedback,
		CancelType:               cancelType,
		CancelledAt:              s.CancelledAt,
		DeletionScheduledAt:      s.DeletionScheduledAt,
		GatewayResponse:          s.Metadata,
		ScheduledPriceID:         s.ScheduledPriceID,
		UpdatedAt:                s.UpdatedAt,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *SubscriptionRepo) Delete(ctx context.Context, id uuid.UUID) error {
	rows, err := r.db.Gen(ctx).DeleteSubscription(ctx, id)
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

// attachSubscriptionRelations stitches Price and PaymentMethod (the bun-era
// selectWithDetails relations) onto subs; withProduct additionally loads
// Price.Product (selectWithProduct).
func (r *SubscriptionRepo) attachSubscriptionRelations(ctx context.Context, subs []*models.Subscription, withProduct bool) error {
	if len(subs) == 0 {
		return nil
	}
	priceIDs := make([]uuid.UUID, 0, len(subs))
	pmIDs := make([]uuid.UUID, 0, len(subs))
	seenPrice := map[uuid.UUID]bool{}
	seenPM := map[uuid.UUID]bool{}
	for _, s := range subs {
		if s.PriceID != uuid.Nil && !seenPrice[s.PriceID] {
			seenPrice[s.PriceID] = true
			priceIDs = append(priceIDs, s.PriceID)
		}
		if s.PaymentMethodID != nil && !seenPM[*s.PaymentMethodID] {
			seenPM[*s.PaymentMethodID] = true
			pmIDs = append(pmIDs, *s.PaymentMethodID)
		}
	}

	q := r.db.Gen(ctx)
	prices := map[uuid.UUID]*models.Price{}
	if len(priceIDs) > 0 {
		if withProduct {
			rows, err := q.ListPricesWithProductByIDs(ctx, priceIDs)
			if err != nil {
				return err
			}
			for _, row := range rows {
				price, err := priceFromGen(row.BillingPrice)
				if err != nil {
					return err
				}
				product, err := productFromGen(row.BillingProduct)
				if err != nil {
					return err
				}
				price.Product = product
				prices[price.ID] = price
			}
		} else {
			rows, err := q.ListPricesByIDs(ctx, priceIDs)
			if err != nil {
				return err
			}
			for _, row := range rows {
				price, err := priceFromGen(row)
				if err != nil {
					return err
				}
				prices[price.ID] = price
			}
		}
	}
	pms := map[uuid.UUID]*models.PaymentMethod{}
	if len(pmIDs) > 0 {
		rows, err := q.ListPaymentMethodsByIDs(ctx, pmIDs)
		if err != nil {
			return err
		}
		for _, row := range rows {
			pm, err := paymentMethodFromGen(row)
			if err != nil {
				return err
			}
			pms[pm.ID] = pm
		}
	}
	for _, s := range subs {
		s.Price = prices[s.PriceID]
		if s.PaymentMethodID != nil {
			s.PaymentMethod = pms[*s.PaymentMethodID]
		}
	}
	return nil
}

// oneWithDetails maps a single gen row and attaches the standard relations.
func (r *SubscriptionRepo) oneWithDetails(ctx context.Context, row gen.BillingSubscription, withProduct bool) (*models.Subscription, error) {
	sub, err := subscriptionFromGen(row)
	if err != nil {
		return nil, err
	}
	if err := r.attachSubscriptionRelations(ctx, []*models.Subscription{sub}, withProduct); err != nil {
		return nil, err
	}
	return sub, nil
}

func (r *SubscriptionRepo) manyWithDetails(ctx context.Context, rows []gen.BillingSubscription) ([]*models.Subscription, error) {
	subs, err := subscriptionsFromGen(rows)
	if err != nil {
		return nil, err
	}
	if err := r.attachSubscriptionRelations(ctx, subs, false); err != nil {
		return nil, err
	}
	return subs, nil
}

func (r *SubscriptionRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Subscription, error) {
	row, err := r.db.Gen(ctx).GetSubscriptionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetLatestByUserID(ctx context.Context, userID string) (*models.Subscription, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetLatestSubscriptionByTenantSubject(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetByUserIDAndPriceID(ctx context.Context, userID string, priceID uuid.UUID) (*models.Subscription, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetSubscriptionByTenantSubjectAndPrice(ctx, gen.GetSubscriptionByTenantSubjectAndPriceParams{
		TenantSubjectID: tsid,
		PriceID:         &priceID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetActiveOrPendingByUserIDAndProductID finds any lifecycle-owning subscription for a user and product.
// Returns the subscription with the latest period end date.
func (r *SubscriptionRepo) GetActiveOrPendingByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetLifecycleSubscriptionByTenantSubjectAndProduct(ctx, gen.GetLifecycleSubscriptionByTenantSubjectAndProductParams{
		TenantSubjectID: tsid,
		ProductID:       productID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetActiveSubscription(ctx context.Context, userID string) (*models.Subscription, error) {
	return r.GetActiveSubscriptionAt(ctx, userID, time.Now())
}

func (r *SubscriptionRepo) GetActiveSubscriptionAt(ctx context.Context, userID string, now time.Time) (*models.Subscription, error) {
	if now.IsZero() {
		now = time.Now()
	}
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetActiveSubscriptionByTenantSubjectAt(ctx, gen.GetActiveSubscriptionByTenantSubjectAtParams{
		TenantSubjectID: tsid,
		Now:             now,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetByProcessorSubscriptionID finds a subscription by processor and processor_subscription_id.
func (r *SubscriptionRepo) GetByProcessorSubscriptionID(ctx context.Context, processor, processorSubscriptionID string) (*models.Subscription, error) {
	row, err := r.db.Gen(ctx).GetSubscriptionByProcessorSubID(ctx, gen.GetSubscriptionByProcessorSubIDParams{
		Processor:               processor,
		ProcessorSubscriptionID: processorSubscriptionID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetByProcessorMetadataValue(ctx context.Context, processor, key, value string) (*models.Subscription, error) {
	row, err := r.db.Gen(ctx).GetSubscriptionByProcessorMetadataValue(ctx, gen.GetSubscriptionByProcessorMetadataValueParams{
		Processor: strings.TrimSpace(processor),
		Key:       strings.TrimSpace(key),
		Value:     strings.TrimSpace(value),
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetActiveSubscriptionsByUserID(ctx context.Context, userID string) ([]models.Subscription, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListActiveSubscriptionsByTenantSubject(ctx, tsid)
	if err != nil {
		return nil, err
	}
	subs, err := r.manyWithDetails(ctx, rows)
	if err != nil {
		return nil, err
	}
	return derefSubs(subs), nil
}

func (r *SubscriptionRepo) GetSubscriptionsByProcessorAndUserID(ctx context.Context, userID string, processor models.Processor) ([]models.Subscription, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListSubscriptionsByTenantSubjectProcessor(ctx, gen.ListSubscriptionsByTenantSubjectProcessorParams{
		TenantSubjectID: tsid,
		Processor:       string(processor),
	})
	if err != nil {
		return nil, err
	}
	subs, err := r.manyWithDetails(ctx, rows)
	if err != nil {
		return nil, err
	}
	return derefSubs(subs), nil
}

func (r *SubscriptionRepo) GetActiveSubscriptionsByProcessor(ctx context.Context, processor string) ([]*models.Subscription, error) {
	rows, err := r.db.Gen(ctx).ListActiveSubscriptionsByProcessor(ctx, processor)
	if err != nil {
		return nil, err
	}
	return r.manyWithDetails(ctx, rows)
}

func (r *SubscriptionRepo) GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	return r.GetSubscriptionsWithDetailsForUser(ctx, userID, page, pageSize)
}

func (r *SubscriptionRepo) GetSubscriptionsWithDetailsForUser(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, 0, err
	}
	q := r.db.Gen(ctx)
	total, err := q.CountSubscriptionsByTenantSubject(ctx, tsid)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.ListSubscriptionsByTenantSubjectPaged(ctx, gen.ListSubscriptionsByTenantSubjectPagedParams{
		TenantSubjectID: tsid,
		PageLimit:       int32(min(pageSize, math.MaxInt32)),
		PageOffset:      int32(min((page-1)*pageSize, math.MaxInt32)),
	})
	if err != nil {
		return nil, 0, err
	}
	subs, err := r.manyWithDetails(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return derefSubs(subs), int(total), nil
}

func (r *SubscriptionRepo) GetSubscribers(ctx context.Context, params query.QueryOptions[SubscriptionFilters]) ([]*models.Subscription, int64, error) {
	f := params.Filters
	tsidResolved, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, f.UserID)
	if err != nil {
		return nil, 0, err
	}
	var tsid *uuid.UUID
	if f.UserID != "" {
		tsid = &tsidResolved
	}
	var status, processor *string
	if f.Status != "" {
		status = &f.Status
	}
	if f.Processor != "" {
		processor = &f.Processor
	}
	var priceID *uuid.UUID
	if f.PriceID != uuid.Nil {
		priceID = &f.PriceID
	}

	q := r.db.Gen(ctx)
	total, err := q.CountSubscriptionsFiltered(ctx, gen.CountSubscriptionsFilteredParams{
		TenantSubjectID: tsid,
		Status:          status,
		PriceID:         priceID,
		Processor:       processor,
		CreatedAfter:    f.CreatedAfter,
		CreatedBefore:   f.CreatedBefore,
		CancelledAfter:  f.CancelledAfter,
		CancelledBefore: f.CancelledBefore,
		ExpiresBefore:   f.ExpiresBefore,
	})
	if err != nil {
		return nil, 0, err
	}

	sortBy := f.SortBy
	switch sortBy {
	case "expires_at", "cancelled_at":
	default:
		sortBy = "created_at"
	}
	rows, err := q.ListSubscriptionsFiltered(ctx, gen.ListSubscriptionsFilteredParams{
		TenantSubjectID: tsid,
		Status:          status,
		PriceID:         priceID,
		Processor:       processor,
		CreatedAfter:    f.CreatedAfter,
		CreatedBefore:   f.CreatedBefore,
		CancelledAfter:  f.CancelledAfter,
		CancelledBefore: f.CancelledBefore,
		ExpiresBefore:   f.ExpiresBefore,
		SortBy:          sortBy,
		SortDesc:        f.SortOrder != "asc",
		PageLimit:       int32(min(params.Limit, math.MaxInt32)),
		PageOffset:      int32(min(params.Offset, math.MaxInt32)),
	})
	if err != nil {
		return nil, 0, err
	}
	subs, err := r.manyWithDetails(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return subs, total, nil
}

// GetActiveOrPendingByUserIDAndTierGroup finds a lifecycle-owning subscription for a user
// where the product belongs to the specified tier group.
// Returns the subscription with its Price and Product loaded.
func (r *SubscriptionRepo) GetActiveOrPendingByUserIDAndTierGroup(ctx context.Context, userID string, tierGroup string) (*models.Subscription, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetLifecycleSubscriptionByTenantSubjectAndTierGroup(ctx, gen.GetLifecycleSubscriptionByTenantSubjectAndTierGroupParams{
		TenantSubjectID: tsid,
		TierGroup:       &tierGroup,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, true)
}

func derefSubs(subs []*models.Subscription) []models.Subscription {
	out := make([]models.Subscription, 0, len(subs))
	for _, s := range subs {
		out = append(out, *s)
	}
	return out
}

func intPtrTo32(v *int) *int32 {
	if v == nil {
		return nil
	}
	i := int32(min(*v, math.MaxInt32))
	return &i
}

// ListDueDunningSubscriptions returns past_due subscriptions on the given
// processors whose next retry is due (Price + PaymentMethod relations
// attached) — the dunning worker's work list.
func (r *SubscriptionRepo) ListDueDunningSubscriptions(ctx context.Context, processors []string, now time.Time) ([]models.Subscription, error) {
	rows, err := r.db.Gen(ctx).ListDueDunningSubscriptions(ctx, gen.ListDueDunningSubscriptionsParams{
		Processors: processors,
		Now:        now,
	})
	if err != nil {
		return nil, err
	}
	subs, err := r.manyWithDetails(ctx, rows)
	if err != nil {
		return nil, err
	}
	out := make([]models.Subscription, 0, len(subs))
	for _, s := range subs {
		out = append(out, *s)
	}
	return out, nil
}

// ListPendingDeletionScheduled returns cancelled subscriptions that still
// carry a deletion_scheduled_at marker — their deferred processor-side delete
// never finalized (#344 follow-up boot rescan). No relations attached.
func (r *SubscriptionRepo) ListPendingDeletionScheduled(ctx context.Context) ([]*models.Subscription, error) {
	rows, err := r.db.Gen(ctx).ListPendingDeletionScheduledSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	return subscriptionsFromGen(rows)
}

// GetLatestResumableCancelled returns the payer's most recent cancelled
// subscription whose paid period has not elapsed (resume candidate), or
// pgx.ErrNoRows.
func (r *SubscriptionRepo) GetLatestResumableCancelled(ctx context.Context, tenantSubjectID uuid.UUID, now time.Time) (*models.Subscription, error) {
	row, err := r.db.Gen(ctx).GetLatestResumableCancelledSubscription(ctx, gen.GetLatestResumableCancelledSubscriptionParams{
		TenantSubjectID: tenantSubjectID,
		Now:             now,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}
