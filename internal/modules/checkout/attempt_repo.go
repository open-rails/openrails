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
	"github.com/open-rails/openrails/internal/merchant"
)

type CheckoutAttemptRepo struct {
	db *db.DB
}

func NewCheckoutAttemptRepo(d *db.DB) *CheckoutAttemptRepo {
	return &CheckoutAttemptRepo{db: d}
}

func checkoutAttemptJSONB(s *models.CheckoutAttempt) (meta, fields, state []byte, err error) {
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

func (r *CheckoutAttemptRepo) Create(ctx context.Context, session *models.CheckoutAttempt) error {
	if err := session.ValidateTerms(); err != nil {
		return err
	}
	var currency *string
	if session.Currency != nil {
		value := strings.TrimSpace(*session.Currency)
		currency = &value
	}
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, session.CustomerID); err != nil {
		return err
	}
	meta, fields, state, err := checkoutAttemptJSONB(session)
	if err != nil {
		return err
	}
	var routingReason []byte
	if session.RoutingReason != nil {
		if routingReason, err = json.Marshal(session.RoutingReason); err != nil {
			return fmt.Errorf("encode checkout attempt routing reason: %w", err)
		}
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	// checkout_attempts.psp_id is NOT NULL; this guard names the reason
	// instead of surfacing a NOT NULL error.
	if session.PspID == uuid.Nil {
		return fmt.Errorf("create checkout attempt %s: %w", session.ID, db.ErrNoPSPInContext)
	}
	rows, err := r.db.Gen(ctx).CreateCheckoutAttempt(ctx, gen.CreateCheckoutAttemptParams{
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

func (r *CheckoutAttemptRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.CheckoutAttempt, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	row, err := r.db.Gen(ctx).GetCheckoutAttemptByID(ctx, gen.GetCheckoutAttemptByIDParams{MerchantID: queryMerchant.UUID(), ID: id})
	if err != nil {
		return nil, err
	}
	return models.CheckoutAttemptFromGen(row)
}

func (r *CheckoutAttemptRepo) Update(ctx context.Context, session *models.CheckoutAttempt) error {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	if err := session.ValidateTerms(); err != nil {
		return err
	}
	// Setup bindings and secret erasure have dedicated conditional writes.
	// Generic checkout progress must not replace accepted capture authority.
	if session.Mode == models.CheckoutAttemptModePaymentMethod {
		return fmt.Errorf("payment-method setup requires captured completion or expiry")
	}
	meta, fields, state, err := checkoutAttemptJSONB(session)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdateCheckoutAttempt(ctx, gen.UpdateCheckoutAttemptParams{MerchantID: queryMerchant.UUID(),
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

func (r *CheckoutAttemptRepo) BindSolanaTransactionRequest(ctx context.Context, session *models.CheckoutAttempt, payer string, now time.Time) error {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	if session == nil {
		return errors.New("checkout attempt is nil")
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		return errors.New("checkout attempt reference is required")
	}
	if session.RailState == nil {
		return errors.New("checkout attempt rail state is required")
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
	rows, err := r.db.Gen(ctx).BindSolanaCheckoutAttempt(ctx, gen.BindSolanaCheckoutAttemptParams{MerchantID: queryMerchant.UUID(),
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
		return fmt.Errorf("solana checkout attempt binding conflict")
	}
	return nil
}

func (r *CheckoutAttemptRepo) GetByReference(ctx context.Context, reference string) (*models.CheckoutAttempt, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	ref := strings.TrimSpace(reference)
	row, err := r.db.Gen(ctx).GetCheckoutAttemptByReference(ctx, gen.GetCheckoutAttemptByReferenceParams{MerchantID: queryMerchant.UUID(), Reference: &ref})
	if err != nil {
		return nil, err
	}
	return models.CheckoutAttemptFromGen(row)
}

func (r *CheckoutAttemptRepo) GetLatestOpenByUserPriceRail(ctx context.Context, userID string, priceID uuid.UUID, rail models.Rail, now time.Time) (*models.CheckoutAttempt, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).GetLatestOpenCheckoutAttempt(ctx, gen.GetLatestOpenCheckoutAttemptParams{MerchantID: queryMerchant.UUID(),
		CustomerID: tsid,
		PriceID:    &priceID,
		Rail:       string(rail),
		Now:        now,
	})
	if err != nil {
		return nil, err
	}
	return models.CheckoutAttemptFromGen(row)
}
