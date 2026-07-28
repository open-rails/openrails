package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

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
// for one merchant. This is a privileged host seam; callers must authorize the
// merchant ID before use.
func (c *ControlPlane) ListPendingPaymentSettlements(ctx context.Context, merchantID merchant.ID, limit int) ([]PaymentSettlement, error) {
	if merchantID.IsZero() {
		return nil, errors.New("control plane: list payment settlements: merchant id is required")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := gen.New(c.pool).ListPendingPaymentSettlements(ctx, gen.ListPendingPaymentSettlementsParams{
		MerchantID: merchantID.UUID(),
		RowLimit:   int64(limit),
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
// returns ErrPaymentSettlementNotFound.
func (c *ControlPlane) AcknowledgePaymentSettlement(ctx context.Context, merchantID merchant.ID, id uuid.UUID) error {
	if merchantID.IsZero() {
		return errors.New("control plane: acknowledge payment settlement: merchant id is required")
	}
	n, err := gen.New(c.pool).AcknowledgePaymentSettlement(ctx, gen.AcknowledgePaymentSettlementParams{
		MerchantID: merchantID.UUID(),
		ID:         id,
	})
	if err != nil {
		return fmt.Errorf("control plane: acknowledge payment settlement: %w", err)
	}
	if n == 0 {
		return ErrPaymentSettlementNotFound
	}
	return nil
}
