package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/uptrace/bun"
)

type CheckoutSessionRepo struct {
	db *db.DB
}

func NewCheckoutSessionRepo(d *db.DB) *CheckoutSessionRepo {
	return &CheckoutSessionRepo{db: d}
}

func (r *CheckoutSessionRepo) Create(ctx context.Context, session *models.CheckoutSession) error {
	res, err := r.db.GetDB().NewInsert().Model(session).Exec(ctx)
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

func (r *CheckoutSessionRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.CheckoutSession, error) {
	session := new(models.CheckoutSession)
	if err := r.db.GetDB().NewSelect().Model(session).Where("cs.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return session, nil
}

func (r *CheckoutSessionRepo) Update(ctx context.Context, session *models.CheckoutSession) error {
	res, err := r.db.GetDB().NewUpdate().Model(session).WherePK().Exec(ctx)
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

func (r *CheckoutSessionRepo) BindSolanaTransactionRequest(ctx context.Context, session *models.CheckoutSession, payer string, now time.Time) error {
	if session == nil {
		return errors.New("checkout session is nil")
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		return errors.New("checkout session reference is required")
	}
	if session.ProcessorState == nil {
		return errors.New("checkout session processor state is required")
	}
	ref := strings.TrimSpace(*session.Reference)
	payer = strings.TrimSpace(payer)
	if payer == "" {
		return errors.New("solana payer is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	session.UpdatedAt = now
	res, err := r.db.GetDB().NewUpdate().
		Model((*models.CheckoutSession)(nil)).
		Set("reference = ?", ref).
		Set("processor_state = ?", session.ProcessorState).
		Set("updated_at = ?", now).
		Where("id = ?", session.ID).
		Where("processor = ?", models.ProcessorSolana).
		Where("status = ?", models.CheckoutSessionStatusRequiresAction).
		Where("(reference IS NULL OR reference = ?)", ref).
		Where("(COALESCE(processor_state ->> 'payer', '') = '' OR processor_state ->> 'payer' = ?)", payer).
		Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return fmt.Errorf("solana checkout session binding conflict")
	}
	return nil
}

func (r *CheckoutSessionRepo) GetByReference(ctx context.Context, reference string) (*models.CheckoutSession, error) {
	session := new(models.CheckoutSession)
	if err := r.db.GetDB().NewSelect().
		Model(session).
		Where("cs.reference = ?", strings.TrimSpace(reference)).
		Limit(1).
		Scan(ctx); err != nil {
		return nil, err
	}
	return session, nil
}

func (r *CheckoutSessionRepo) GetLatestOpenByUserPriceProcessor(ctx context.Context, userID string, priceID uuid.UUID, processor models.Processor) (*models.CheckoutSession, error) {
	session := new(models.CheckoutSession)
	openStatuses := []models.CheckoutSessionStatus{
		models.CheckoutSessionStatusCreated,
		models.CheckoutSessionStatusRequiresAction,
	}
	if err := r.db.GetDB().NewSelect().
		Model(session).
		Where("cs.user_id = ?", userID).
		Where("cs.price_id = ?", priceID).
		Where("cs.processor = ?", processor).
		Where("cs.status IN (?)", bun.In(openStatuses)).
		Where("(cs.expires_at IS NULL OR cs.expires_at > ?)", time.Now()).
		OrderExpr("cs.created_at DESC").
		Limit(1).
		Scan(ctx); err != nil {
		return nil, err
	}
	return session, nil
}
