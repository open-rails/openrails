package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

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

// ListPendingPaymentSettlements returns the oldest unacknowledged settlements.
func (c *ControlPlane) ListPendingPaymentSettlements(ctx context.Context, limit int) ([]PaymentSettlement, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := gen.New(c.pool).ListPendingPaymentSettlements(ctx, int64(limit))
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
func (c *ControlPlane) AcknowledgePaymentSettlement(ctx context.Context, id uuid.UUID) error {
	if err := gen.New(c.pool).AcknowledgePaymentSettlement(ctx, id); err != nil {
		return fmt.Errorf("control plane: acknowledge payment settlement: %w", err)
	}
	return nil
}
