package controlplane

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/app"
	internalcp "github.com/open-rails/openrails/internal/controlplane"
)

// PaymentSettlement is a durable successful-payment event for an embedding
// host.
type PaymentSettlement = internalcp.PaymentSettlement

// ListPendingPaymentSettlements returns successful payments not yet
// acknowledged by the embedding host.
func ListPendingPaymentSettlements(ctx context.Context, a *app.App, limit int) ([]PaymentSettlement, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	return cp.ListPendingPaymentSettlements(ctx, limit)
}

// AcknowledgePaymentSettlement marks one event as processed by the embedding
// host.
func AcknowledgePaymentSettlement(ctx context.Context, a *app.App, id uuid.UUID) error {
	cp := Get(a)
	if cp == nil {
		return fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	return cp.AcknowledgePaymentSettlement(ctx, id)
}
