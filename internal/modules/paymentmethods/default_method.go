package paymentmethods

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// A customer with any usable stored payment method has exactly one default
// (#1084). The database keeps the invariant on every write: at most one by a
// partial unique index, at least one by a deferred trigger that promotes the
// most recently used card when the default is removed or parked, so no save,
// delete, park, import or restore path needs to maintain it by hand.

// ErrPaymentMethodNotUsable refuses making a parked (or pending-delete) method
// the default.
var ErrPaymentMethodNotUsable = errors.New("payment method cannot be the default: it is not usable")

// DefaultPaymentMethodID returns the customer's default method, if any.
func (s *PaymentMethodService) DefaultPaymentMethodID(ctx context.Context, customerID uuid.UUID) (uuid.UUID, bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	id, err := s.repo.db.Gen(ctx).GetDefaultPaymentMethodID(ctx, gen.GetDefaultPaymentMethodIDParams{MerchantID: mid.UUID(), CustomerID: customerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

// SetDefaultPaymentMethod makes one of the customer's usable methods the
// default, atomically replacing the previous one.
func (s *PaymentMethodService) SetDefaultPaymentMethod(ctx context.Context, customerID, methodID uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.repo.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.repo.db.NewWithPgxTx(tx).Gen(ctx)
		if err := q.LockCustomerDefaultPaymentMethod(ctx, gen.LockCustomerDefaultPaymentMethodParams{MerchantID: mid.UUID(), CustomerID: customerID}); err != nil {
			return err
		}
		method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: methodID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && method.CustomerID != customerID) {
			return ErrPaymentMethodNotFound
		}
		if err != nil {
			return err
		}
		if method.ParkReason != "" {
			return ErrPaymentMethodNotUsable
		}
		if err := q.ClearDefaultPaymentMethod(ctx, gen.ClearDefaultPaymentMethodParams{MerchantID: mid.UUID(), CustomerID: customerID, KeepID: methodID}); err != nil {
			return err
		}
		n, err := q.MarkDefaultPaymentMethod(ctx, gen.MarkDefaultPaymentMethodParams{MerchantID: mid.UUID(), CustomerID: customerID, ID: methodID})
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrPaymentMethodNotUsable
		}
		return nil
	})
}
