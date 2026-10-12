package intents

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

func (s *Store) enqueueSale(ctx context.Context, p EnqueueParams) (gen.BillingProviderIntent, error) {
	var row gen.BillingProviderIntent
	mid, err := merchant.Require(ctx)
	if err != nil || mid.UUID() != p.MerchantID {
		return row, errors.New("sale merchant does not match context")
	}
	raw, err := json.Marshal(p.Payload)
	if err != nil {
		return row, err
	}
	target := p.PspID
	if target == uuid.Nil {
		target, err = db.RequirePSPID(ctx)
		if err != nil {
			return row, err
		}
	}
	// Read only the requested lock coordinates here. Full canonical validation
	// runs against the real inserted/existing row before this transaction commits.
	var terms payments.NMISalePayload
	if err := json.Unmarshal(raw, &terms); err != nil {
		return row, err
	}
	customer, err := uuid.Parse(terms.UserID)
	order := terms.OrderID != uuid.Nil
	if err != nil || customer == uuid.Nil || order != (p.PriceID == nil) || !order && *p.PriceID != terms.PriceID || terms.Instrument.PSPID != target {
		return row, errors.New("sale admission coordinates contradict requested purchase")
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: p.MerchantID, ID: customer}); err != nil {
			return err
		}
		store := s.withTxDB(d)
		previous, err := store.GetByIdempotencyKey(ctx, p.IdempotencyKey)
		if err == nil {
			accepted, err := payments.DecodeNMISalePayload(previous)
			if err != nil {
				return err
			}
			if accepted.CheckoutAttemptID != terms.CheckoutAttemptID || accepted.UserID != terms.UserID || accepted.PriceID != terms.PriceID || accepted.RequestFingerprint != terms.RequestFingerprint || accepted.PaymentMethodID != terms.PaymentMethodID || accepted.Instrument.PSPID != terms.Instrument.PSPID {
				return apperr.Conflictf("checkout key belongs to another accepted purchase")
			}
			row = previous
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !order {
			return apperr.Conflictf("priced checkout creation is retired; an accepted operation must already exist")
		}
		// An order's new card is saved by the sale itself: no method exists yet.
		if !terms.NewCard {
			method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: p.MerchantID, ID: terms.PaymentMethodID})
			if err != nil {
				return err
			}
			if method.CustomerID != customer || method.Rail != p.Provider || !paymentmethods.Chargeable(method) {
				return apperr.Conflictf("sale instrument changed before admission")
			}
			if err := terms.Instrument.Matches(method); err != nil {
				return err
			}
			if err := mandates.Recheck(ctx, d.Gen(ctx), p.MerchantID, terms.Instrument.Mandate); err != nil {
				return err
			}
		}
		row, err = store.enqueue(ctx, p)
		if err != nil {
			return err
		}
		accepted, err := payments.DecodeNMISalePayload(row)
		if err != nil {
			return err
		}
		if accepted.CheckoutAttemptID != terms.CheckoutAttemptID || accepted.UserID != terms.UserID || accepted.PriceID != terms.PriceID || accepted.RequestFingerprint != terms.RequestFingerprint || accepted.PaymentMethodID != terms.PaymentMethodID || accepted.Instrument.PSPID != terms.Instrument.PSPID {
			return apperr.Conflictf("checkout key belongs to another accepted purchase")
		}
		return nil
	})
	return row, err
}
