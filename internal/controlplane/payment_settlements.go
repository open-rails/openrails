package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrPaymentSettlementNotFound indicates the settlement id does not exist
// under the given merchant scope.
var ErrPaymentSettlementNotFound = errors.New("controlplane: payment settlement not found for merchant")

// PaymentSettlement is a durable successful-payment event for an embedding
// host. A host should acknowledge it only after its idempotent processing
// succeeds.
type PaymentSettlement struct {
	ID         uuid.UUID
	MerchantID merchant.ID
	PaymentID  uuid.UUID
	Amount     int64
	Currency   string
	SettledAt  time.Time
}

// ListPendingPaymentSettlements returns the oldest unacknowledged settlements
// for one merchant. This is a host seam; callers must authorize the merchant ID
// before use.
//
// or#861: this ran on the bare pool. payment_settlement_events gained RLS in
// migration 0010 and the feed was never re-pointed, so under openrails_app the
// WHERE clause was ANDed with `merchant_id = NULL` and the feed listed NOTHING
// — a host-facing data-loss bug that every test missed because the tests ran on
// a GUC-bearing connection. The scope is pinned explicitly: the caller names
// the merchant, MerchantTx pins the GUC transaction-locally, and the query's
// own merchant_id predicate stays as defence in depth.
func (c *ControlPlane) ListPendingPaymentSettlements(ctx context.Context, merchantID merchant.ID, limit int) ([]PaymentSettlement, error) {
	if merchantID.IsZero() {
		return nil, errors.New("control plane: list payment settlements: merchant id is required")
	}
	if limit <= 0 {
		limit = 100
	}
	var rows []gen.ListPendingPaymentSettlementsRow
	err := c.pool.MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		var qErr error
		rows, qErr = gen.New(tx).ListPendingPaymentSettlements(ctx, gen.ListPendingPaymentSettlementsParams{
			MerchantID: merchantID.UUID(),
			RowLimit:   int64(limit),
		})
		return qErr
	})
	if err != nil {
		return nil, fmt.Errorf("control plane: list payment settlements: %w", err)
	}

	settlements := make([]PaymentSettlement, 0, len(rows))
	for _, row := range rows {
		settlements = append(settlements, PaymentSettlement{
			ID:         row.ID,
			MerchantID: merchant.ID(row.MerchantID),
			PaymentID:  row.PaymentID,
			Amount:     row.Amount,
			Currency:   row.Currency,
			SettledAt:  row.SettledAt,
		})
	}
	return settlements, nil
}

// AcknowledgePaymentSettlement removes a settlement from the pending feed.
// The id must belong to the given merchant; acking another merchant's event
// returns ErrPaymentSettlementNotFound. Merchant-scoped for the same reason as
// the list above (or#861) — on the bare pool every ack matched zero rows and so
// returned not-found regardless of the id.
func (c *ControlPlane) AcknowledgePaymentSettlement(ctx context.Context, merchantID merchant.ID, id uuid.UUID) error {
	if merchantID.IsZero() {
		return errors.New("control plane: acknowledge payment settlement: merchant id is required")
	}
	var n int64
	err := c.pool.MerchantTx(ctx, merchantID, func(ctx context.Context, tx pgx.Tx) error {
		var qErr error
		n, qErr = gen.New(tx).AcknowledgePaymentSettlement(ctx, gen.AcknowledgePaymentSettlementParams{
			MerchantID: merchantID.UUID(),
			ID:         id,
		})
		return qErr
	})
	if err != nil {
		return fmt.Errorf("control plane: acknowledge payment settlement: %w", err)
	}
	if n == 0 {
		return ErrPaymentSettlementNotFound
	}
	return nil
}
