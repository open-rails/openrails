package payment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

type OrderAttemptRepo struct {
	db *db.DB
}

func NewOrderAttemptRepo(d *db.DB) *OrderAttemptRepo {
	return &OrderAttemptRepo{db: d}
}

func orderAttemptJSONB(s *models.OrderAttempt) (meta, fields, state []byte, err error) {
	if meta, err = models.ToJSONB(s.Metadata); err != nil {
		return nil, nil, nil, err
	}
	if fields, err = models.ToJSONB(s.RailFields); err != nil {
		return nil, nil, nil, err
	}
	if state, err = models.ToJSONB(s.RailState); err != nil {
		return nil, nil, nil, err
	}
	return meta, fields, state, nil
}

func (r *OrderAttemptRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.OrderAttempt, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	row, err := r.db.Gen(ctx).GetOrderAttemptByID(ctx, gen.GetOrderAttemptByIDParams{MerchantID: queryMerchant.UUID(), ID: id})
	if err != nil {
		return nil, err
	}
	return models.OrderAttemptFromGen(row)
}

func (r *OrderAttemptRepo) Update(ctx context.Context, session *models.OrderAttempt) error {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	if err := session.ValidateTerms(); err != nil {
		return err
	}
	meta, fields, state, err := orderAttemptJSONB(session)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdateOrderAttempt(ctx, gen.UpdateOrderAttemptParams{MerchantID: queryMerchant.UUID(),
		ID:         session.ID,
		CustomerID: session.CustomerID,

		Rail:           string(session.Rail),
		Status:         string(session.Status),
		Amount:         session.Amount,
		Currency:       session.Currency,
		ExpiresAt:      session.ExpiresAt,
		Reference:      session.Reference,
		TransactionID:  session.TransactionID,
		PaymentID:      session.PaymentID,
		SubscriptionID: session.SubscriptionID,
		Metadata:       meta,
		RailFields:     fields,
		RailState:      state,
		PspID:          session.PspID,
		UpdatedAt:      models.UpdateTimestamp(session.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *OrderAttemptRepo) BindSolanaTransactionRequest(ctx context.Context, session *models.OrderAttempt, payer string, now time.Time) error {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	if session == nil {
		return errors.New("order attempt is nil")
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		return errors.New("order attempt reference is required")
	}
	if session.RailState == nil {
		return errors.New("order attempt rail state is required")
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
	state, err := models.ToJSONB(session.RailState)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).BindSolanaOrderAttempt(ctx, gen.BindSolanaOrderAttemptParams{MerchantID: queryMerchant.UUID(),
		ID:        session.ID,
		Reference: &ref,
		RailState: state,
		UpdatedAt: now,
		Payer:     payer,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return fmt.Errorf("solana order attempt binding conflict")
	}
	return nil
}

func (r *OrderAttemptRepo) GetByReference(ctx context.Context, reference string) (*models.OrderAttempt, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	ref := strings.TrimSpace(reference)
	row, err := r.db.Gen(ctx).GetOrderAttemptByReference(ctx, gen.GetOrderAttemptByReferenceParams{MerchantID: queryMerchant.UUID(), Reference: &ref})
	if err != nil {
		return nil, err
	}
	return models.OrderAttemptFromGen(row)
}
