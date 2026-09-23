package intents

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

func (s *Store) enqueueSale(ctx context.Context, p EnqueueParams) (gen.OpenrailsRailIntent, error) {
	var row gen.OpenrailsRailIntent
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
	if err != nil || customer == uuid.Nil || p.PriceID == nil || *p.PriceID != terms.PriceID || terms.Instrument.PSPID != target {
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
			if accepted.CheckoutSessionID != terms.CheckoutSessionID || accepted.UserID != terms.UserID || accepted.PriceID != terms.PriceID || accepted.RequestFingerprint != terms.RequestFingerprint || accepted.PaymentMethodID != terms.PaymentMethodID || accepted.Instrument.PSPID != terms.Instrument.PSPID {
				return apperr.Conflictf("checkout key belongs to another accepted purchase")
			}
			row = previous
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if terms.AccessDurationHours == nil {
			pending, err := d.Gen(ctx).HasUnresolvedProductCheckout(ctx, gen.HasUnresolvedProductCheckoutParams{MerchantID: p.MerchantID, CustomerID: customer, ProductID: terms.ProductID, ExceptSessionID: terms.CheckoutSessionID})
			if err != nil {
				return err
			}
			if pending {
				return apperr.Conflictf("another checkout of this product is unresolved")
			}
		}
		_, err = d.Gen(ctx).GetUnresolvedSaleForCustomerProduct(ctx, gen.GetUnresolvedSaleForCustomerProductParams{MerchantID: p.MerchantID, CustomerID: customer.String(), ProductID: terms.ProductID.String()})
		if err == nil {
			return apperr.Conflictf("another purchase of this product is unresolved")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: p.MerchantID, ID: terms.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != customer || method.Rail != p.Provider || method.ParkReason != "" {
			return apperr.Conflictf("sale instrument changed before admission")
		}
		if err := terms.Instrument.Matches(method, charge.AgreementUnscheduled); err != nil {
			return err
		}
		row, err = store.enqueue(ctx, p)
		if err != nil {
			return err
		}
		accepted, err := payments.DecodeNMISalePayload(row)
		if err != nil {
			return err
		}
		if accepted.CheckoutSessionID != terms.CheckoutSessionID || accepted.UserID != terms.UserID || accepted.PriceID != terms.PriceID || accepted.RequestFingerprint != terms.RequestFingerprint || accepted.PaymentMethodID != terms.PaymentMethodID || accepted.Instrument.PSPID != terms.Instrument.PSPID {
			return apperr.Conflictf("checkout key belongs to another accepted purchase")
		}
		return nil
	})
	return row, err
}
