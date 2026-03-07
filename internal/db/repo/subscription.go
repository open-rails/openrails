package repo

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/query"
	"github.com/uptrace/bun"
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

func (r *SubscriptionRepo) Create(ctx context.Context, s *models.Subscription) error {
	res, err := r.db.GetDB().NewInsert().Model(s).Exec(ctx)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rows < 1 {
		return errors.New("no rows affected")
	}

	return nil
}

func (r *SubscriptionRepo) Update(ctx context.Context, s *models.Subscription) error {
	// Note: We explicitly list all columns to ensure nil values are set correctly.
	// Bun's default behavior with nullzero tags skips nil fields, which prevents
	// clearing fields like CancelledAt, EndedAt when reactivating subscriptions.
	s.UpdatedAt = time.Now()
	res, err := r.db.GetDB().NewUpdate().Model(s).
		Column(
			"status",
			"started_at",
			"ended_at",
			"current_period_starts_at",
			"current_period_ends_at",
			"processor",
			"processor_subscription_id",
			"user_email",
			"payment_method_id",
			"last_retry_at",
			"retry_attempts",
			"next_retry_at",
			"grace_ends_at",
			"cancel_feedback",
			"cancel_type",
			"cancelled_at",
			"gateway_response",
			"scheduled_price_id",
			"updated_at",
		).
		WherePK().
		Exec(ctx)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rows < 1 {
		return errors.New("no rows affected")
	}

	return nil
}

func (r *SubscriptionRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := r.db.GetDB().NewDelete().Model((*models.Subscription)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rows < 1 {
		return errors.New("no rows affected")
	}

	return nil
}

func (r *SubscriptionRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Subscription, error) {
	sub := new(models.Subscription)
	err := r.selectWithDetails(sub).Where("sub.id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (r *SubscriptionRepo) GetLatestByUserID(ctx context.Context, userID string) (*models.Subscription, error) {
	sub := new(models.Subscription)
	err := r.selectWithDetails(sub).
		Where("sub.user_id = ?", userID).
		Order("sub.created_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (r *SubscriptionRepo) GetByUserIDAndPriceID(ctx context.Context, userID string, priceID uuid.UUID) (*models.Subscription, error) {
	sub := new(models.Subscription)
	err := r.selectWithDetails(sub).
		Where("sub.user_id = ?", userID).
		Where("sub.price_id = ?", priceID).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// GetActiveOrPendingByUserIDAndProductID finds any active or pending subscription for a user and product.
// Returns the subscription with the latest period end date.
func (r *SubscriptionRepo) GetActiveOrPendingByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error) {
	sub := new(models.Subscription)
	err := r.selectWithDetails(sub).
		Where("sub.user_id = ?", userID).
		Where("sub.product_id = ?", productID).
		Where("sub.status IN (?, ?)", models.StatusActive, models.StatusPending).
		Order("sub.current_period_ends_at DESC NULLS FIRST"). // NULLS FIRST prioritizes indefinite subscriptions
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (r *SubscriptionRepo) GetActiveSubscription(ctx context.Context, userID string) (*models.Subscription, error) {
	sub := new(models.Subscription)
	err := r.selectWithDetails(sub).
		Where("sub.user_id = ?", userID).
		Where("sub.status = ?", models.StatusActive).
		Where("(sub.current_period_ends_at IS NULL OR sub.current_period_ends_at > NOW())").
		Order("sub.created_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// GetByProcessorSubscriptionID finds a subscription by processor and processor_subscription_id.
func (r *SubscriptionRepo) GetByProcessorSubscriptionID(ctx context.Context, processor, processorSubscriptionID string) (*models.Subscription, error) {
	sub := new(models.Subscription)
	query := r.selectWithDetails(sub).
		Where("sub.processor = ?", processor).
		Where("sub.processor_subscription_id = ?", processorSubscriptionID)

	err := query.Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (r *SubscriptionRepo) GetActiveSubscriptionsByUserID(ctx context.Context, userID string) ([]models.Subscription, error) {
	subs := []models.Subscription{}
	query := r.selectWithDetails(&subs).
		Where("sub.user_id = ?", userID).
		Where("sub.status = ?", models.StatusActive).
		Order("sub.created_at DESC")

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}
	return subs, nil
}

func (r *SubscriptionRepo) GetSubscriptionsByProcessorAndUserID(ctx context.Context, userID string, processor models.Processor) ([]models.Subscription, error) {
	subs := []models.Subscription{}
	query := r.selectWithDetails(&subs).
		Where("sub.user_id = ?", userID).
		Where("sub.processor = ?", processor).
		Order("sub.created_at DESC")

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}
	return subs, nil
}

func (r *SubscriptionRepo) GetActiveSubscriptionsByProcessor(ctx context.Context, processor string) ([]*models.Subscription, error) {
	subs := []*models.Subscription{}
	query := r.selectWithDetails(&subs).
		Where("sub.processor = ?", processor).
		Where("sub.status = ?", models.StatusActive)

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}
	return subs, nil
}

func (r *SubscriptionRepo) GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	offset := (page - 1) * pageSize
	countQuery := r.db.GetDB().NewSelect().Model((*models.Subscription)(nil)).Where("sub.user_id = ?", userID)
	total, err := countQuery.Count(ctx)
	if err != nil {
		return nil, 0, err
	}

	subs := []models.Subscription{}
	dataQuery := r.selectWithDetails(&subs).
		Where("sub.user_id = ?", userID).
		Order("sub.created_at DESC").
		Limit(pageSize).
		Offset(offset)

	if err := dataQuery.Scan(ctx); err != nil {
		return nil, 0, err
	}

	return subs, total, nil
}

func (r *SubscriptionRepo) GetSubscriptionsWithDetailsForUser(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	offset := (page - 1) * pageSize
	countQuery := r.db.GetDB().NewSelect().Model((*models.Subscription)(nil)).Where("sub.user_id = ?", userID)
	total, err := countQuery.Count(ctx)
	if err != nil {
		return nil, 0, err
	}

	subs := []models.Subscription{}
	dataQuery := r.selectWithDetails(&subs).
		Where("sub.user_id = ?", userID).
		Order("sub.created_at DESC").
		Limit(pageSize).
		Offset(offset)

	if err := dataQuery.Scan(ctx); err != nil {
		return nil, 0, err
	}

	return subs, total, nil
}

func (r *SubscriptionRepo) GetSubscribers(ctx context.Context, params query.QueryOptions[SubscriptionFilters]) ([]*models.Subscription, int64, error) {
	base := r.db.GetDB().NewSelect().Model((*models.Subscription)(nil))
	base = applySubscriptionFilters(base, params.Filters)

	total, err := base.Count(ctx)
	if err != nil {
		return nil, 0, err
	}

	subs := []*models.Subscription{}
	dataQuery := r.selectWithDetails(&subs)
	dataQuery = applySubscriptionFilters(dataQuery, params.Filters)

	if params.Limit > 0 {
		dataQuery = dataQuery.Limit(params.Limit)
	}
	if params.Offset > 0 {
		dataQuery = dataQuery.Offset(params.Offset)
	}

	// Apply sorting
	dataQuery = applySorting(dataQuery, params.Filters.SortBy, params.Filters.SortOrder)

	if err := dataQuery.Scan(ctx); err != nil {
		return nil, 0, err
	}

	return subs, int64(total), nil
}

func applySorting(q *bun.SelectQuery, sortBy, sortOrder string) *bun.SelectQuery {
	// Validate and map sort field
	var column string
	switch sortBy {
	case "expires_at":
		column = "sub.current_period_ends_at"
	case "cancelled_at":
		column = "sub.cancelled_at"
	default:
		column = "sub.created_at"
	}

	// Validate sort order
	order := "DESC"
	if sortOrder == "asc" {
		order = "ASC"
	}

	return q.OrderExpr(column + " " + order)
}

func applySubscriptionFilters(q *bun.SelectQuery, filters SubscriptionFilters) *bun.SelectQuery {
	if filters.UserID != "" {
		q = q.Where("sub.user_id = ?", filters.UserID)
	}
	if filters.Status != "" {
		q = q.Where("sub.status = ?", filters.Status)
	}
	if filters.PriceID != uuid.Nil {
		q = q.Where("sub.price_id = ?", filters.PriceID)
	}
	if filters.Processor != "" {
		q = q.Where("sub.processor = ?", filters.Processor)
	}
	if filters.CreatedAfter != nil {
		q = q.Where("sub.created_at >= ?", filters.CreatedAfter)
	}
	if filters.CreatedBefore != nil {
		q = q.Where("sub.created_at <= ?", filters.CreatedBefore)
	}
	if filters.CancelledAfter != nil {
		q = q.Where("sub.cancelled_at >= ?", filters.CancelledAfter)
	}
	if filters.CancelledBefore != nil {
		q = q.Where("sub.cancelled_at <= ?", filters.CancelledBefore)
	}
	if filters.ExpiresBefore != nil {
		q = q.Where("sub.current_period_ends_at <= ?", filters.ExpiresBefore)
	}
	return q
}

func (r *SubscriptionRepo) selectWithDetails(model any) *bun.SelectQuery {
	return r.db.GetDB().NewSelect().Model(model).
		Relation("Price").
		Relation("PaymentMethod")
}

func (r *SubscriptionRepo) selectWithProduct(model any) *bun.SelectQuery {
	return r.db.GetDB().NewSelect().Model(model).
		Relation("Price").
		Relation("Price.Product").
		Relation("PaymentMethod")
}

// GetActiveOrPendingByUserIDAndTierGroup finds an active or pending subscription for a user
// where the product belongs to the specified tier group.
// Returns the subscription with its Price and Product loaded.
func (r *SubscriptionRepo) GetActiveOrPendingByUserIDAndTierGroup(ctx context.Context, userID string, tierGroup string) (*models.Subscription, error) {
	sub := new(models.Subscription)
	err := r.selectWithProduct(sub).
		Where("sub.user_id = ?", userID).
		Where("sub.status IN (?, ?)", models.StatusActive, models.StatusPending).
		// Join to products table to filter by tier_group
		Join("JOIN billing.products AS prod ON prod.id = sub.product_id").
		Where("prod.tier_group = ?", tierGroup).
		Order("sub.current_period_ends_at DESC NULLS FIRST"). // Prioritize indefinite subscriptions
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}
