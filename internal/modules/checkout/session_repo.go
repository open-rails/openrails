package checkout

import (
	"context"
	"encoding/json"
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

func (r *CheckoutSessionRepo) Create(ctx context.Context, session *models.CheckoutSession) error {
	currency := strings.TrimSpace(session.Currency)
	if currency == "" {
		return fmt.Errorf("checkout session currency required")
	}
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, session.CustomerID); err != nil {
		return err
	}
	meta, fields, state, err := checkoutSessionJSONB(session)
	if err != nil {
		return err
	}
	var routingReason []byte
	if session.RoutingReason != nil {
		if routingReason, err = json.Marshal(session.RoutingReason); err != nil {
			return fmt.Errorf("encode checkout session routing reason: %w", err)
		}
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	// or#893: checkout_sessions.psp_id is NOT NULL. The service refuses an
	// unroutable rail before it builds a session; this is the repo's own guard
	// so the failure names the reason rather than surfacing as a NOT NULL error.
	if session.PspID == uuid.Nil {
		return fmt.Errorf("create checkout session %s: %w", session.ID, db.ErrNoPSPInContext)
	}
	rows, err := r.db.Gen(ctx).CreateCheckoutSession(ctx, gen.CreateCheckoutSessionParams{
		ID:             session.ID,
		MerchantID:     tid.UUID(),
		CustomerID:     session.CustomerID,
		PriceID:        session.PriceID,
		Mode:           string(session.Mode),
		Rail:           string(session.Rail),
		Status:         string(session.Status),
		Amount:         session.Amount,
		Currency:       currency,
		ExpiresAt:      session.ExpiresAt,
		Reference:      session.Reference,
		TransactionID:  session.TransactionID,
		PaymentID:      session.PaymentID,
		SubscriptionID: session.SubscriptionID,
		Metadata:       meta,
		RailFields:     fields,
		RailState:      state,
		RoutingReason:  routingReason,
		PspID:          session.PspID,
		CreatedAt:      session.CreatedAt,
		UpdatedAt:      session.UpdatedAt,
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
	return models.CheckoutSessionFromGen(row)
}

func (r *CheckoutSessionRepo) Update(ctx context.Context, session *models.CheckoutSession) error {
	meta, fields, state, err := checkoutSessionJSONB(session)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdateCheckoutSession(ctx, gen.UpdateCheckoutSessionParams{
		ID:             session.ID,
		CustomerID:     session.CustomerID,
		PriceID:        session.PriceID,
		Mode:           string(session.Mode),
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

func (r *CheckoutSessionRepo) BindSolanaTransactionRequest(ctx context.Context, session *models.CheckoutSession, payer string, now time.Time) error {
	if session == nil {
		return errors.New("checkout session is nil")
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		return errors.New("checkout session reference is required")
	}
	if session.RailState == nil {
		return errors.New("checkout session rail state is required")
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
	rows, err := r.db.Gen(ctx).BindSolanaCheckoutSession(ctx, gen.BindSolanaCheckoutSessionParams{
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
	return models.CheckoutSessionFromGen(row)
}

func (r *CheckoutSessionRepo) GetLatestOpenByUserPriceRail(ctx context.Context, userID string, priceID uuid.UUID, rail models.Rail) (*models.CheckoutSession, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetLatestOpenCheckoutSession(ctx, gen.GetLatestOpenCheckoutSessionParams{
		CustomerID: tsid,
		PriceID:    priceID,
		Rail:       string(rail),
		Now:        time.Now(),
	})
	if err != nil {
		return nil, err
	}
	return models.CheckoutSessionFromGen(row)
}
