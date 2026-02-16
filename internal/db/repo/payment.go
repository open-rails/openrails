package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/query"
	"github.com/uptrace/bun"
)

type PaymentFilters struct {
	UserID         string     `form:"user_id"`
	PriceID        uuid.UUID  `form:"price_id"`
	SubscriptionID string     `form:"subscription_id"` // UUID string, parsed in handler
	Processor      string     `form:"processor"`
	TransactionID  string     `form:"transaction_id"`
	StartDate      *time.Time `form:"created_after" time_format:"2006-01-02"`
	EndDate        *time.Time `form:"created_before" time_format:"2006-01-02"`
	MinAmount      *int64     `form:"min_amount"`
	MaxAmount      *int64     `form:"max_amount"`
	RefundsOnly    bool       `form:"refunds_only"`
	SortBy         string     `form:"sort_by"`    // created_at (default), amount, purchased_at
	SortOrder      string     `form:"sort_order"` // asc, desc (default)
}

type PaymentRepo struct {
	db *db.DB
}

func NewPaymentRepo(d *db.DB) *PaymentRepo { return &PaymentRepo{db: d} }

func (r *PaymentRepo) Create(ctx context.Context, payment *models.Payment) error {
	res, err := r.db.GetDB().NewInsert().Model(payment).Exec(ctx)
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

func (r *PaymentRepo) CreateIfNotExists(ctx context.Context, payment *models.Payment) (bool, error) {
	res, err := r.db.GetDB().
		NewInsert().
		Model(payment).
		On("CONFLICT (processor, transaction_id) DO NOTHING").
		Exec(ctx)
	if err != nil {
		return false, err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	return rows > 0, nil
}

func (r *PaymentRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Payment, error) {
	payment := new(models.Payment)
	if err := r.db.GetDB().NewSelect().Model(payment).Where("purch.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return payment, nil
}

// GetByIDWithDetails fetches a payment with all related entities (Price, Product, Subscription)
// and also loads any refund entries linked to this payment
func (r *PaymentRepo) GetByIDWithDetails(ctx context.Context, id uuid.UUID) (*models.Payment, []*models.Payment, error) {
	payment := new(models.Payment)
	err := r.db.GetDB().NewSelect().
		Model(payment).
		Relation("Price").
		Relation("Price.Product").
		Relation("Subscription").
		Where("purch.id = ?", id).
		Scan(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Load any refund entries linked to this payment
	refunds := []*models.Payment{}
	err = r.db.GetDB().NewSelect().
		Model(&refunds).
		Where("refunded_payment_id = ?", id).
		OrderExpr("created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, nil, err
	}

	return payment, refunds, nil
}

func (r *PaymentRepo) GetByUserID(ctx context.Context, userID string) ([]*models.Payment, error) {
	payments := []*models.Payment{}
	if err := r.db.GetDB().NewSelect().Model(&payments).Where("purch.user_id = ?", userID).OrderExpr("purch.purchased_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	return payments, nil
}

func (r *PaymentRepo) GetByTransactionID(ctx context.Context, processor models.Processor, transactionID string) (*models.Payment, error) {
	payment := new(models.Payment)
	if err := r.db.GetDB().NewSelect().Model(payment).Where("purch.processor = ?", processor).Where("purch.transaction_id = ?", transactionID).Scan(ctx); err != nil {
		return nil, err
	}
	return payment, nil
}

func (r *PaymentRepo) GetByPriceID(ctx context.Context, priceID uuid.UUID) ([]*models.Payment, error) {
	payments := []*models.Payment{}
	if err := r.db.GetDB().NewSelect().Model(&payments).Where("purch.price_id = ?", priceID).OrderExpr("purch.purchased_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	return payments, nil
}

func (r *PaymentRepo) GetByProcessor(ctx context.Context, processor models.Processor) ([]*models.Payment, error) {
	payments := []*models.Payment{}
	if err := r.db.GetDB().NewSelect().Model(&payments).Where("purch.processor = ?", processor).OrderExpr("purch.purchased_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	return payments, nil
}

func (r *PaymentRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := r.db.GetDB().NewDelete().Model((*models.Payment)(nil)).Where("purch.id = ?", id).Exec(ctx)
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

func (r *PaymentRepo) GetRefundTotalByPaymentID(ctx context.Context, paymentID uuid.UUID) (int64, error) {
	var total sql.NullInt64
	if err := r.db.GetDB().NewSelect().
		Model((*models.Payment)(nil)).
		ColumnExpr("COALESCE(SUM(ABS(purch.amount)), 0)").
		Where("purch.refunded_payment_id = ?", paymentID).
		Scan(ctx, &total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

func (r *PaymentRepo) GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]*models.Payment, int, error) {
	payments := []*models.Payment{}
	offset := (page - 1) * pageSize

	count, err := r.db.GetDB().NewSelect().Model((*models.Payment)(nil)).Where("purch.user_id = ?", userID).Count(ctx)
	if err != nil {
		return nil, 0, err
	}

	if err := r.db.GetDB().NewSelect().Model(&payments).Where("purch.user_id = ?", userID).OrderExpr("purch.purchased_at DESC").Limit(pageSize).Offset(offset).Scan(ctx); err != nil {
		return nil, 0, err
	}

	return payments, count, nil
}

func (r *PaymentRepo) GetPayments(ctx context.Context, opts query.QueryOptions[PaymentFilters]) ([]*models.Payment, int64, error) {
	payments := []*models.Payment{}
	q := r.db.GetDB().NewSelect().Model(&payments)

	q = q.Relation("Price").Relation("Price.Product").Relation("Subscription")

	if opts.Filters.UserID != "" {
		q = q.Where("purch.user_id = ?", opts.Filters.UserID)
	}
	if opts.Filters.PriceID != uuid.Nil {
		q = q.Where("purch.price_id = ?", opts.Filters.PriceID)
	}
	if opts.Filters.SubscriptionID != "" {
		subID, err := api.ParseSubscriptionID(opts.Filters.SubscriptionID)
		if err == nil {
			q = q.Where("purch.subscription_id = ?", subID)
		}
	}
	if opts.Filters.Processor != "" {
		q = q.Where("purch.processor = ?", opts.Filters.Processor)
	}
	if opts.Filters.TransactionID != "" {
		q = q.Where("purch.transaction_id = ?", opts.Filters.TransactionID)
	}
	if opts.Filters.StartDate != nil {
		q = q.Where("purch.purchased_at >= ?", opts.Filters.StartDate)
	}
	if opts.Filters.EndDate != nil {
		q = q.Where("purch.purchased_at <= ?", opts.Filters.EndDate)
	}
	if opts.Filters.MinAmount != nil {
		q = q.Where("purch.amount >= ?", opts.Filters.MinAmount)
	}
	if opts.Filters.MaxAmount != nil {
		q = q.Where("purch.amount <= ?", opts.Filters.MaxAmount)
	}
	if opts.Filters.RefundsOnly {
		q = q.Where("purch.refunded_payment_id IS NOT NULL")
	}

	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}

	// Apply sorting
	q = applyPaymentSorting(q, opts.Filters.SortBy, opts.Filters.SortOrder)
	q = q.Limit(opts.GetLimit()).Offset(opts.GetOffset())

	if err := q.Scan(ctx); err != nil {
		return nil, 0, err
	}

	return payments, int64(total), nil
}

func (r *PaymentRepo) GetLatestByUserAndProcessor(ctx context.Context, userID string, processor models.Processor) (*models.Payment, error) {
	payment := new(models.Payment)
	err := r.db.GetDB().
		NewSelect().
		Model(payment).
		Where("purch.user_id = ?", userID).
		Where("purch.processor = ?", processor).
		OrderExpr("purch.purchased_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return payment, nil
}

func (r *PaymentRepo) GetLatestBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.Payment, error) {
	payment := new(models.Payment)
	err := r.db.GetDB().
		NewSelect().
		Model(payment).
		Where("purch.subscription_id = ?", subscriptionID).
		OrderExpr("purch.purchased_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return payment, nil
}

func (r *PaymentRepo) CountByUserAndProcessor(ctx context.Context, userID string, processor models.Processor) (successful int, failed int, err error) {
	if userID == "" {
		return 0, 0, fmt.Errorf("user_id is required")
	}

	// Successful payments (positive amount)
	successful, err = r.db.GetDB().
		NewSelect().
		Model((*models.Payment)(nil)).
		Where("purch.user_id = ?", userID).
		Where("purch.processor = ?", processor).
		Where("purch.amount > 0").
		Count(ctx)
	if err != nil {
		return
	}

	// Failed/negative/zero payments
	failed, err = r.db.GetDB().
		NewSelect().
		Model((*models.Payment)(nil)).
		Where("purch.user_id = ?", userID).
		Where("purch.processor = ?", processor).
		Where("purch.amount <= 0").
		Count(ctx)
	if err != nil {
		return
	}

	return
}

func (r *PaymentRepo) MarkFailed(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.GetDB().
		NewUpdate().
		Model((*models.Payment)(nil)).
		Set("status = ?", "failed").
		Where("purch.id = ?", id).
		Exec(ctx)
	return err
}

func applyPaymentSorting(q *bun.SelectQuery, sortBy, sortOrder string) *bun.SelectQuery {
	// Validate and map sort field
	var column string
	switch sortBy {
	case "amount":
		column = "purch.amount"
	case "purchased_at":
		column = "purch.purchased_at"
	default:
		column = "purch.created_at"
	}

	// Validate sort order
	order := "DESC"
	if sortOrder == "asc" {
		order = "ASC"
	}

	return q.OrderExpr(column + " " + order)
}
