package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

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
	rows, err := c.pool.Query(ctx, `
		SELECT id, merchant_id, payment_id, amount, currency, settled_at
		  FROM openrails.payment_settlement_events
		 WHERE delivered_at IS NULL
		 ORDER BY id
		 LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("control plane: list payment settlements: %w", err)
	}
	defer rows.Close()

	settlements := make([]PaymentSettlement, 0, limit)
	for rows.Next() {
		var settlement PaymentSettlement
		var merchantID uuid.UUID
		if err := rows.Scan(&settlement.ID, &merchantID, &settlement.PaymentID, &settlement.Amount, &settlement.Currency, &settlement.SettledAt); err != nil {
			return nil, fmt.Errorf("control plane: scan payment settlement: %w", err)
		}
		settlement.MerchantID = merchant.ID(merchantID)
		settlements = append(settlements, settlement)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("control plane: iterate payment settlements: %w", err)
	}
	return settlements, nil
}

// AcknowledgePaymentSettlement removes a settlement from the pending feed.
func (c *ControlPlane) AcknowledgePaymentSettlement(ctx context.Context, id uuid.UUID) error {
	if _, err := c.pool.Exec(ctx, `
		UPDATE openrails.payment_settlement_events
		   SET delivered_at = COALESCE(delivered_at, now())
		 WHERE id = $1
	`, id); err != nil {
		return fmt.Errorf("control plane: acknowledge payment settlement: %w", err)
	}
	return nil
}
