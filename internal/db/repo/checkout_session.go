package repo

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
	"github.com/open-rails/openrails/pkg/merchant"
)

type CheckoutSessionRepo struct {
	db *db.DB
}

func NewCheckoutSessionRepo(d *db.DB) *CheckoutSessionRepo {
	return &CheckoutSessionRepo{db: d}
}

func checkoutSessionJSONB(s *models.CheckoutSession) (meta, fields, state []byte, err error) {
	if meta, err = toJSONB(s.Metadata); err != nil {
		return nil, nil, nil, err
	}
	if fields, err = toJSONB(s.ProcessorFields); err != nil {
		return nil, nil, nil, err
	}
	if state, err = toJSONB(s.ProcessorState); err != nil {
		return nil, nil, nil, err
	}
	return meta, fields, state, nil
}

func (r *CheckoutSessionRepo) Create(ctx context.Context, session *models.CheckoutSession) error {
	if err := ensureMerchantSubjectRow(ctx, r.db.Qx(ctx), uuid.Nil, session.MerchantSubjectID); err != nil {
		return err
	}
	meta, fields, state, err := checkoutSessionJSONB(session)
	if err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).CreateCheckoutSession(ctx, gen.CreateCheckoutSessionParams{
		ID:              session.ID,
		MerchantID:        tid.UUID(),
		MerchantSubjectID: session.MerchantSubjectID,
		PriceID:         session.PriceID,
		Mode:            string(session.Mode),
		Processor:       string(session.Processor),
		Status:          string(session.Status),
		Amount:          session.Amount,
		Currency:        session.Currency,
		ExpiresAt:       session.ExpiresAt,
		Reference:       session.Reference,
		TransactionID:   session.TransactionID,
		PaymentID:       session.PaymentID,
		SubscriptionID:  session.SubscriptionID,
		Metadata:        meta,
		ProcessorFields: fields,
		ProcessorState:  state,
		IdempotencyKey:  session.IdempotencyKey,
		CreatedAt:       session.CreatedAt,
		UpdatedAt:       session.UpdatedAt,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *CheckoutSessionRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.CheckoutSession, error) {
	row, err := r.db.Gen(ctx).GetCheckoutSessionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return checkoutSessionFromGen(row)
}

func (r *CheckoutSessionRepo) Update(ctx context.Context, session *models.CheckoutSession) error {
	meta, fields, state, err := checkoutSessionJSONB(session)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdateCheckoutSession(ctx, gen.UpdateCheckoutSessionParams{
		ID:              session.ID,
		MerchantSubjectID: session.MerchantSubjectID,
		PriceID:         session.PriceID,
		Mode:            string(session.Mode),
		Processor:       string(session.Processor),
		Status:          string(session.Status),
		Amount:          session.Amount,
		Currency:        session.Currency,
		ExpiresAt:       session.ExpiresAt,
		Reference:       session.Reference,
		TransactionID:   session.TransactionID,
		PaymentID:       session.PaymentID,
		SubscriptionID:  session.SubscriptionID,
		Metadata:        meta,
		ProcessorFields: fields,
		ProcessorState:  state,
		IdempotencyKey:  session.IdempotencyKey,
		UpdatedAt:       updateTimestamp(session.UpdatedAt),
	})
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
	state, err := toJSONB(session.ProcessorState)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).BindSolanaCheckoutSession(ctx, gen.BindSolanaCheckoutSessionParams{
		ID:             session.ID,
		Reference:      &ref,
		ProcessorState: state,
		UpdatedAt:      now,
		Payer:          payer,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return fmt.Errorf("solana checkout session binding conflict")
	}
	return nil
}

func (r *CheckoutSessionRepo) GetByReference(ctx context.Context, reference string) (*models.CheckoutSession, error) {
	ref := strings.TrimSpace(reference)
	row, err := r.db.Gen(ctx).GetCheckoutSessionByReference(ctx, &ref)
	if err != nil {
		return nil, err
	}
	return checkoutSessionFromGen(row)
}

func (r *CheckoutSessionRepo) GetLatestOpenByUserPriceProcessor(ctx context.Context, userID string, priceID uuid.UUID, processor models.Processor) (*models.CheckoutSession, error) {
	tsid, err := ResolveMerchantSubjectID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetLatestOpenCheckoutSession(ctx, gen.GetLatestOpenCheckoutSessionParams{
		MerchantSubjectID: tsid,
		PriceID:         priceID,
		Processor:       string(processor),
		Now:             time.Now(),
	})
	if err != nil {
		return nil, err
	}
	return checkoutSessionFromGen(row)
}
