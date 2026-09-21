package intents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Replacement admission joins deletion's customer/method order. Once an
// update is accepted its existing payload pins the method; once deletion is
// accepted a fresh token must never reach that vendor target.
func (s *Store) enqueueNMIMethodUpdate(ctx context.Context, p EnqueueParams) (gen.OpenrailsRailIntent, error) {
	var row gen.OpenrailsRailIntent
	mid, err := merchant.Require(ctx)
	if err != nil || mid.UUID() != p.MerchantID {
		return row, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	raw, err := json.Marshal(p.Payload)
	if err != nil {
		return row, err
	}
	terms, err := decodeNMIPaymentMethodUpdatePayload(gen.OpenrailsRailIntent{Payload: raw})
	if err != nil {
		return row, err
	}
	customer, err := uuid.Parse(terms.UserID)
	if err != nil || customer == uuid.Nil || p.IdempotencyKey != NMIPaymentMethodUpdateIdempotencyKey(terms.PaymentMethodID, terms.PaymentToken) {
		return row, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		store := s.withTxDB(d)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: p.MerchantID, ID: customer}); err != nil {
			return err
		}
		method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: p.MerchantID, ID: terms.PaymentMethodID})
		if err != nil {
			return err
		}
		if p.Provider != "nmi" || method.Rail != p.Provider || method.CustomerID != customer || method.PspID != p.PspID || method.Custodian != models.CustodianPSP || method.RailCustomerRef != terms.RailCustomerRef || method.RailMethodRef != terms.RailMethodRef {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		prior, err := store.GetByIdempotencyKey(ctx, p.IdempotencyKey)
		if err == nil {
			if prior.IntentType != TypeNMIPaymentMethodUpdate || prior.PspID == nil || *prior.PspID != p.PspID || prior.CustodianID != nil || prior.Rail != p.Provider {
				return paymentmethods.ErrPaymentMethodDeleteUnsafe
			}
			if len(prior.Payload) > 0 {
				accepted, err := decodeNMIPaymentMethodUpdatePayload(prior)
				if err != nil {
					return err
				}
				if accepted.PaymentMethodID != terms.PaymentMethodID || accepted.UserID != terms.UserID || accepted.RailCustomerRef != terms.RailCustomerRef || accepted.RailMethodRef != terms.RailMethodRef {
					return paymentmethods.ErrPaymentMethodDeleteUnsafe
				}
			}
			row = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if strings.HasPrefix(method.ParkReason, "delete:") {
			return paymentmethods.ErrPaymentMethodDeleteProcessing
		}
		row, err = store.enqueue(ctx, p)
		return err
	})
	return row, err
}
