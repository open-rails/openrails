package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/uptrace/bun"
)

type PaymentMethodRepo struct {
	db *db.DB
}

func NewPaymentMethodRepo(d *db.DB) *PaymentMethodRepo { return &PaymentMethodRepo{db: d} }

var (
	ErrPaymentMethodNotFound = errors.New("payment method not found")
)

func (r *PaymentMethodRepo) Create(ctx context.Context, m *models.PaymentMethod) error {
	if err := ensureTenantSubjectRow(ctx, r.db.Q(ctx), uuid.Nil, m.TenantSubjectID); err != nil {
		return err
	}
	res, err := r.db.Q(ctx).NewInsert().Model(m).Exec(ctx)
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

func (r *PaymentMethodRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.PaymentMethod, error) {
	pm := new(models.PaymentMethod)
	err := r.db.Q(ctx).NewSelect().Model(pm).
		Where("pm.id = ?", id).
		Relation("Subscriptions").
		Relation("Subscriptions.Product").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("payment method %s: %w", id, ErrPaymentMethodNotFound)
		}
		return nil, err
	}
	return pm, nil
}

func (r *PaymentMethodRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := r.db.Q(ctx).NewDelete().Model((*models.PaymentMethod)(nil)).Where("pm.id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rows < 1 {
		return ErrPaymentMethodNotFound
	}

	return nil
}

func (r *PaymentMethodRepo) GetByUserID(ctx context.Context, userID string) ([]*models.PaymentMethod, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	methods := []*models.PaymentMethod{}
	err = r.db.Q(ctx).NewSelect().Model(&methods).
		Where("pm.tenant_subject_id = ?", tsid).
		OrderExpr("pm.created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return methods, nil
}

func (r *PaymentMethodRepo) ListByUserID(ctx context.Context, userID string, limit, offset int) ([]*models.PaymentMethod, int64, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, 0, err
	}
	countQuery := r.db.Q(ctx).NewSelect().Model((*models.PaymentMethod)(nil)).
		Where("pm.tenant_subject_id = ?", tsid)

	total, err := countQuery.Count(ctx)
	if err != nil {
		return nil, 0, err
	}

	methods := []*models.PaymentMethod{}
	dataQuery := r.db.Q(ctx).NewSelect().Model(&methods).
		Where("pm.tenant_subject_id = ?", tsid).
		Relation("Subscriptions").
		Relation("Subscriptions.Product").
		OrderExpr("pm.created_at DESC")

	if limit > 0 {
		dataQuery.Limit(limit)
	}
	if offset > 0 {
		dataQuery.Offset(offset)
	}

	if err := dataQuery.Scan(ctx); err != nil {
		return nil, 0, err
	}

	return methods, int64(total), nil
}

func (r *PaymentMethodRepo) GetByVaultID(ctx context.Context, processor, vaultID string) (*models.PaymentMethod, error) {
	pm := new(models.PaymentMethod)

	query := r.db.Q(ctx).NewSelect().Model(pm).
		Where("pm.processor = ?", processor).
		Where("pm.vault_id = ?", vaultID)

	err := query.Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPaymentMethodNotFound
		}
		return nil, err
	}
	return pm, nil
}

func (r *PaymentMethodRepo) GetByInitialTransactionID(ctx context.Context, processor, initialTransactionID string) (*models.PaymentMethod, error) {
	pm := new(models.PaymentMethod)

	query := r.db.Q(ctx).NewSelect().Model(pm).
		Where("pm.processor = ?", processor).
		Where("pm.initial_transaction_id = ?", initialTransactionID)

	err := query.Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPaymentMethodNotFound
		}
		return nil, err
	}
	return pm, nil
}

func (r *PaymentMethodRepo) Update(ctx context.Context, method *models.PaymentMethod) error {
	res, err := r.db.Q(ctx).NewUpdate().Model(method).WherePK().Exec(ctx)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rows < 1 {
		return ErrPaymentMethodNotFound
	}

	return nil
}

// GetAllNMIBacked returns all payment methods for NMI-backed processors
func (r *PaymentMethodRepo) GetAllNMIBacked(ctx context.Context) ([]*models.PaymentMethod, error) {
	nmiProcessors := processors.GetNMIBackedProcessorsList()
	methods := []*models.PaymentMethod{}
	err := r.db.Q(ctx).NewSelect().Model(&methods).
		Where("pm.processor IN (?)", bun.In(nmiProcessors)).
		OrderExpr("pm.created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return methods, nil
}

// GetNMIBackedByUserID returns all payment methods for NMI-backed processors for a user
func (r *PaymentMethodRepo) GetNMIBackedByUserID(ctx context.Context, userID string) ([]*models.PaymentMethod, error) {
	nmiProcessors := processors.GetNMIBackedProcessorsList()
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	methods := []*models.PaymentMethod{}
	if err := r.db.Q(ctx).NewSelect().Model(&methods).
		Where("pm.tenant_subject_id = ?", tsid).
		Where("pm.processor IN (?)", bun.In(nmiProcessors)).
		OrderExpr("pm.created_at DESC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return methods, nil
}

func (r *PaymentMethodRepo) ExistsForUser(ctx context.Context, id uuid.UUID, userID string) (bool, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return false, err
	}
	count, err := r.db.Q(ctx).NewSelect().
		Model((*models.PaymentMethod)(nil)).
		Where("pm.id = ?", id).
		Where("pm.tenant_subject_id = ?", tsid).
		Count(ctx)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *PaymentMethodRepo) WithTx(txdb *db.DB) *PaymentMethodRepo {
	return NewPaymentMethodRepo(txdb)
}

func (r *PaymentMethodRepo) GetByProcessor(ctx context.Context, processor models.Processor) ([]*models.PaymentMethod, error) {
	methods := []*models.PaymentMethod{}
	err := r.db.Q(ctx).NewSelect().Model(&methods).
		Where("pm.processor = ?", processor).
		OrderExpr("pm.created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return methods, nil
}

func (r *PaymentMethodRepo) RequireByID(ctx context.Context, id uuid.UUID) (*models.PaymentMethod, error) {
	pm, err := r.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return pm, nil
}
