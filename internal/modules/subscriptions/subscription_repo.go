package subscriptions

import (
	"context"
	"errors"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/query"
)

type SubscriptionFilters struct {
	UserID          string
	Status          string
	PriceID         uuid.UUID
	Rail            string
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
	entSnap, err := models.ToJSONB(s.EntitlementsSpecSnapshot)
	if err != nil {
		return gen.CreateSubscriptionParams{}, err
	}
	credSnap, err := models.ToJSONB(s.CreditsSpecSnapshot)
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
		CustomerID:               s.CustomerID,
		ProductID:                s.ProductID,
		PriceID:                  priceID,
		ScheduledPriceID:         s.ScheduledPriceID,
		EntitlementsSpecSnapshot: entSnap,
		CreditsSpecSnapshot:      credSnap,
		Status:                   string(s.Status),
		RailMerchantAccountID:    s.RailMerchantAccountID,
		StartedAt:                s.StartedAt,
		EndedAt:                  s.EndedAt,
		CurrentPeriodStartsAt:    s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:      s.CurrentPeriodEndsAt,
		Rail:                     string(s.Rail),
		RailSubscriptionID:       s.RailSubscriptionID,
		UserEmail:                s.UserEmail,
		PaymentMethodID:          s.PaymentMethodID,
		LastRetryAt:              s.LastRetryAt,
		RetryAttempts:            models.IntPtrTo32(s.RetryAttempts),
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
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, s.CustomerID); err != nil {
		return err
	}
	params, err := subscriptionInsertParams(s)
	if err != nil {
		return err
	}
	tid, terr := merchant.Require(ctx)
	if terr != nil {
		return terr
	}
	params.MerchantID = tid.UUID()
	if params.RailMerchantAccountID == nil {
		params.RailMerchantAccountID = db.ResolveRailMerchantAccountIDForStamp(ctx)
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

// ReplaceForTierChange atomically persists a tier change: it writes oldSub
// (pre-mutated by the caller to its cancelled state) and inserts newSub in ONE
// transaction. The partial unique index
// uq_subscriptions_customer_tier_group_active allows only one live
// subscription per (tenant_subject, tier_group), so the old row's cancel and
// the new row's insert must commit together — and the cancel must execute
// first. On any failure the transaction rolls back and the old subscription
// remains active locally (SEC-10: upgrade compensation never has to
// reactivate it).
func (r *SubscriptionRepo) ReplaceForTierChange(ctx context.Context, oldSub, newSub *models.Subscription, now time.Time) error {
	return r.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txRepo := NewSubscriptionRepo(r.db.NewWithPgxTx(tx))
		if err := txRepo.UpdateAt(ctx, oldSub, now); err != nil {
			return err
		}
		return txRepo.Create(ctx, newSub)
	})
}

func (r *SubscriptionRepo) UpdateAt(ctx context.Context, s *models.Subscription, now time.Time) error {
	// All columns are written explicitly so nil values CLEAR fields
	// (CancelledAt, EndedAt, ...) when reactivating subscriptions. Because this
	// is a full-row write from an in-memory image, webhook-apply
	// read-modify-writes must read via GetByRailSubscriptionIDForUpdate inside
	// one tx or a concurrent writer's committed changes get reverted (#675).
	if now.IsZero() {
		now = time.Now()
	}
	s.UpdatedAt = now

	entSnap, err := models.ToJSONB(s.EntitlementsSpecSnapshot)
	if err != nil {
		return err
	}
	credSnap, err := models.ToJSONB(s.CreditsSpecSnapshot)
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
		Status:                   gen.OpenrailsSubscriptionStatus(s.Status),
		StartedAt:                s.StartedAt,
		EndedAt:                  s.EndedAt,
		CurrentPeriodStartsAt:    s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:      s.CurrentPeriodEndsAt,
		Rail:                     string(s.Rail),
		RailSubscriptionID:       s.RailSubscriptionID,
		UserEmail:                s.UserEmail,
		PaymentMethodID:          s.PaymentMethodID,
		LastRetryAt:              s.LastRetryAt,
		RetryAttempts:            models.IntPtrTo32(s.RetryAttempts),
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
				price, err := models.PriceFromGen(row.OpenrailsPrice)
				if err != nil {
					return err
				}
				product, err := models.ProductFromGen(row.OpenrailsProduct)
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
				price, err := models.PriceFromGen(row)
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
			pm, err := models.PaymentMethodFromGen(row)
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
func (r *SubscriptionRepo) oneWithDetails(ctx context.Context, row gen.OpenrailsSubscription, withProduct bool) (*models.Subscription, error) {
	sub, err := models.SubscriptionFromGen(row)
	if err != nil {
		return nil, err
	}
	if err := r.attachSubscriptionRelations(ctx, []*models.Subscription{sub}, withProduct); err != nil {
		return nil, err
	}
	return sub, nil
}

func (r *SubscriptionRepo) manyWithDetails(ctx context.Context, rows []gen.OpenrailsSubscription) ([]*models.Subscription, error) {
	subs, err := models.SubscriptionsFromGen(rows)
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
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetLatestSubscriptionByCustomer(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetByUserIDAndPriceID(ctx context.Context, userID string, priceID uuid.UUID) (*models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetSubscriptionByCustomerAndPrice(ctx, gen.GetSubscriptionByCustomerAndPriceParams{
		CustomerID: tsid,
		PriceID:    &priceID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetActiveOrPendingByUserIDAndProductID finds any lifecycle-owning subscription for a user and product.
// Returns the subscription with the latest period end date.
func (r *SubscriptionRepo) GetActiveOrPendingByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetLifecycleSubscriptionByCustomerAndProduct(ctx, gen.GetLifecycleSubscriptionByCustomerAndProductParams{
		CustomerID: tsid,
		ProductID:  productID,
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
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetActiveSubscriptionByCustomerAt(ctx, gen.GetActiveSubscriptionByCustomerAtParams{
		CustomerID: tsid,
		Now:        now,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetByRailSubscriptionIDForUpdate is the row-locked (FOR UPDATE) variant for
// webhook-apply read-modify-writes (#675). Must run inside a transaction;
// UpdateAt is a full-row write, so the lock must be held from read to write.
func (r *SubscriptionRepo) GetByRailSubscriptionIDForUpdate(ctx context.Context, rail, railSubscriptionID string) (*models.Subscription, error) {
	row, err := r.db.Gen(ctx).GetSubscriptionByRailSubIDForUpdate(ctx, gen.GetSubscriptionByRailSubIDForUpdateParams{
		Rail:               rail,
		RailSubscriptionID: railSubscriptionID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetByRailSubscriptionID finds a subscription by rail and rail_subscription_id.
func (r *SubscriptionRepo) GetByRailSubscriptionID(ctx context.Context, rail, railSubscriptionID string) (*models.Subscription, error) {
	row, err := r.db.Gen(ctx).GetSubscriptionByRailSubID(ctx, gen.GetSubscriptionByRailSubIDParams{
		Rail:               rail,
		RailSubscriptionID: railSubscriptionID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetByRailMetadataValue(ctx context.Context, rail, key, value string) (*models.Subscription, error) {
	row, err := r.db.Gen(ctx).GetSubscriptionByRailMetadataValue(ctx, gen.GetSubscriptionByRailMetadataValueParams{
		Rail:  strings.TrimSpace(rail),
		Key:   strings.TrimSpace(key),
		Value: strings.TrimSpace(value),
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

func (r *SubscriptionRepo) GetActiveSubscriptionsByUserID(ctx context.Context, userID string) ([]models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListActiveSubscriptionsByCustomer(ctx, tsid)
	if err != nil {
		return nil, err
	}
	subs, err := r.manyWithDetails(ctx, rows)
	if err != nil {
		return nil, err
	}
	return derefSubs(subs), nil
}

func (r *SubscriptionRepo) GetSubscriptionsByRailAndUserID(ctx context.Context, userID string, rail models.Rail) ([]models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListSubscriptionsByCustomerRail(ctx, gen.ListSubscriptionsByCustomerRailParams{
		CustomerID: tsid,
		Rail:       string(rail),
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

func (r *SubscriptionRepo) GetActiveSubscriptionsByRail(ctx context.Context, rail string) ([]*models.Subscription, error) {
	rows, err := r.db.Gen(ctx).ListActiveSubscriptionsByRail(ctx, rail)
	if err != nil {
		return nil, err
	}
	return r.manyWithDetails(ctx, rows)
}

func (r *SubscriptionRepo) GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	return r.GetSubscriptionsWithDetailsForUser(ctx, userID, page, pageSize)
}

func (r *SubscriptionRepo) GetSubscriptionsWithDetailsForUser(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, 0, err
	}
	q := r.db.Gen(ctx)
	total, err := q.CountSubscriptionsByCustomer(ctx, tsid)
	if err != nil {
		return nil, 0, err
	}
	pageSize32, _ := safecast.Convert[int32](pageSize)
	pageOffset32, _ := safecast.Convert[int32]((page - 1) * pageSize)
	rows, err := q.ListSubscriptionsByCustomerPaged(ctx, gen.ListSubscriptionsByCustomerPagedParams{
		CustomerID: tsid,
		PageLimit:  pageSize32,
		PageOffset: pageOffset32,
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
	tsidResolved, err := db.ResolveCustomerID(f.UserID)
	if err != nil {
		return nil, 0, err
	}
	var tsid *uuid.UUID
	if f.UserID != "" {
		tsid = &tsidResolved
	}
	var status, rail *string
	if f.Status != "" {
		status = &f.Status
	}
	if f.Rail != "" {
		rail = &f.Rail
	}
	var priceID *uuid.UUID
	if f.PriceID != uuid.Nil {
		priceID = &f.PriceID
	}

	q := r.db.Gen(ctx)
	total, err := q.CountSubscriptionsFiltered(ctx, gen.CountSubscriptionsFilteredParams{
		CustomerID:      tsid,
		Status:          status,
		PriceID:         priceID,
		Rail:            rail,
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
	paramsLimit32, _ := safecast.Convert[int32](params.Limit)
	paramsOffset32, _ := safecast.Convert[int32](params.Offset)
	rows, err := q.ListSubscriptionsFiltered(ctx, gen.ListSubscriptionsFilteredParams{
		CustomerID:      tsid,
		Status:          status,
		PriceID:         priceID,
		Rail:            rail,
		CreatedAfter:    f.CreatedAfter,
		CreatedBefore:   f.CreatedBefore,
		CancelledAfter:  f.CancelledAfter,
		CancelledBefore: f.CancelledBefore,
		ExpiresBefore:   f.ExpiresBefore,
		SortBy:          sortBy,
		SortDesc:        f.SortOrder != "asc",
		PageLimit:       paramsLimit32,
		PageOffset:      paramsOffset32,
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
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetLifecycleSubscriptionByCustomerAndTierGroup(ctx, gen.GetLifecycleSubscriptionByCustomerAndTierGroupParams{
		CustomerID: tsid,
		TierGroup:  &tierGroup,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, true)
}

// GetUnknownByUserIDAndProductID finds an `unknown`-status subscription for a
// user and product (#691 checkout guard): parked pending provider verification,
// possibly still alive/billing at the provider.
func (r *SubscriptionRepo) GetUnknownByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetUnknownSubscriptionByCustomerAndProduct(ctx, gen.GetUnknownSubscriptionByCustomerAndProductParams{
		CustomerID: tsid,
		ProductID:  productID,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}

// GetUnknownByUserIDAndTierGroup is the tier-group variant of the #691 checkout
// guard lookup. Returns the subscription with Price and Product loaded.
func (r *SubscriptionRepo) GetUnknownByUserIDAndTierGroup(ctx context.Context, userID string, tierGroup string) (*models.Subscription, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetUnknownSubscriptionByCustomerAndTierGroup(ctx, gen.GetUnknownSubscriptionByCustomerAndTierGroupParams{
		CustomerID: tsid,
		TierGroup:  &tierGroup,
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

// ListDueDunningSubscriptions returns past_due subscriptions on the given
// rails whose next retry is due (Price + PaymentMethod relations
// attached) — the dunning worker's work list.
func (r *SubscriptionRepo) ListDueDunningSubscriptions(ctx context.Context, rails []string, now time.Time) ([]models.Subscription, error) {
	rows, err := r.db.Gen(ctx).ListDueDunningSubscriptions(ctx, gen.ListDueDunningSubscriptionsParams{
		Rails: rails,
		Now:   now,
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
// carry a deletion_scheduled_at marker — their deferred rail-side delete
// never finalized (#344 follow-up boot rescan). No relations attached.
func (r *SubscriptionRepo) ListPendingDeletionScheduled(ctx context.Context) ([]*models.Subscription, error) {
	rows, err := r.db.Gen(ctx).ListPendingDeletionScheduledSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	return models.SubscriptionsFromGen(rows)
}

// GetLatestResumableCancelled returns the payer's most recent cancelled
// subscription whose paid period has not elapsed (resume candidate), or
// pgx.ErrNoRows.
func (r *SubscriptionRepo) GetLatestResumableCancelled(ctx context.Context, tenantSubjectID uuid.UUID, now time.Time) (*models.Subscription, error) {
	row, err := r.db.Gen(ctx).GetLatestResumableCancelledSubscription(ctx, gen.GetLatestResumableCancelledSubscriptionParams{
		CustomerID: tenantSubjectID,
		Now:        now,
	})
	if err != nil {
		return nil, err
	}
	return r.oneWithDetails(ctx, row, false)
}
