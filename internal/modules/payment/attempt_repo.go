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

type InvoiceAttemptRepo struct {
	db *db.DB
}

func NewInvoiceAttemptRepo(d *db.DB) *InvoiceAttemptRepo {
	return &InvoiceAttemptRepo{db: d}
}

func invoiceAttemptJSONB(s *models.InvoiceAttempt) (meta, fields, state []byte, err error) {
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

func (r *InvoiceAttemptRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.InvoiceAttempt, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	row, err := r.db.Gen(ctx).GetInvoiceAttemptByID(ctx, gen.GetInvoiceAttemptByIDParams{MerchantID: queryMerchant.UUID(), ID: id})
	if err != nil {
		return nil, err
	}
	return models.InvoiceAttemptFromGen(row)
}

func (r *InvoiceAttemptRepo) Update(ctx context.Context, session *models.InvoiceAttempt) error {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	if err := session.ValidateTerms(); err != nil {
		return err
	}
	meta, fields, state, err := invoiceAttemptJSONB(session)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdateInvoiceAttempt(ctx, gen.UpdateInvoiceAttemptParams{MerchantID: queryMerchant.UUID(),
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

func (r *InvoiceAttemptRepo) BindSolanaTransactionRequest(ctx context.Context, session *models.InvoiceAttempt, payer string, now time.Time) error {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	if session == nil {
		return errors.New("invoice attempt is nil")
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		return errors.New("invoice attempt reference is required")
	}
	if session.RailState == nil {
		return errors.New("invoice attempt rail state is required")
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
	rows, err := r.db.Gen(ctx).BindSolanaInvoiceAttempt(ctx, gen.BindSolanaInvoiceAttemptParams{MerchantID: queryMerchant.UUID(),
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
		return fmt.Errorf("solana invoice attempt binding conflict")
	}
	return nil
}
