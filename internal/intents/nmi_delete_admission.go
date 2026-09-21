package intents

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/merchant"
)

func (s *Store) enqueueNMIMethodDelete(ctx context.Context, p EnqueueParams) (gen.OpenrailsRailIntent, error) {
	var row gen.OpenrailsRailIntent
	mid, err := merchant.Require(ctx)
	if err != nil || mid.UUID() != p.MerchantID {
		return row, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	raw, err := json.Marshal(p.Payload)
	if err != nil {
		return row, err
	}
	var terms NMIPaymentMethodDeletePayload
	if err = json.Unmarshal(raw, &terms); err != nil {
		return row, err
	}
	customer, err := uuid.Parse(terms.UserID)
	if err != nil || customer == uuid.Nil || terms.PaymentMethodID == uuid.Nil || p.IdempotencyKey != NMIPaymentMethodDeleteIdempotencyKey(terms.PaymentMethodID) || p.CustodianID != uuid.Nil || p.SubscriptionID != nil || p.PaymentID != nil || p.PriceID != nil || p.ExpiresAt != nil {
		return row, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	if err := validatePaymentMethodDeleteAuthority(ctx, p); err != nil {
		return row, err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		store := s.withTxDB(d)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: p.MerchantID, ID: customer}); err != nil {
			return err
		}
		prior, err := store.GetByIdempotencyKey(ctx, p.IdempotencyKey)
		if err == nil {
			accepted, err := decodeNMIVaultDeletePayload(prior)
			if err != nil {
				return err
			}
			if accepted != terms || prior.PspID == nil || *prior.PspID != p.PspID || prior.Rail != p.Provider {
				return paymentmethods.ErrPaymentMethodDeleteUnsafe
			}
			row = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		method, err := q.LockPaymentMethodForCustodyRemap(ctx, gen.LockPaymentMethodForCustodyRemapParams{MerchantID: p.MerchantID, ID: terms.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != customer || method.Custodian != models.CustodianPSP || method.CustodianID != nil || method.PspID != p.PspID || method.Rail != p.Provider || method.RailCustomerRef != terms.RailCustomerRef || method.RailMethodRef != terms.RailMethodRef {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		if err := deletionMethodUnused(ctx, q, p.MerchantID, method.ID, 0); err != nil {
			return err
		}
		row, err = store.enqueue(ctx, p)
		if err != nil {
			return err
		}
		n, err := q.FencePaymentMethodDeletion(ctx, gen.FencePaymentMethodDeletionParams{MerchantID: p.MerchantID, ID: method.ID, OperationID: row.ID, Now: p.NextAttemptAt})
		if err != nil {
			return err
		}
		if n != 1 {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		return nil
	})
	return row, err
}
