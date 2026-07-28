package controlplane

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/app"
	internalcp "github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PaymentSettlement is a durable successful-payment event for an embedding
// host.
type PaymentSettlement = internalcp.PaymentSettlement

// ErrPaymentSettlementNotFound indicates the settlement id does not exist
// under the given merchant scope.
var ErrPaymentSettlementNotFound = internalcp.ErrPaymentSettlementNotFound

// ListPendingPaymentSettlements returns successful payments not yet
// acknowledged by the embedding host, scoped to one merchant. This is a
// privileged host seam; callers must authorize the merchant ID before use.
func ListPendingPaymentSettlements(ctx context.Context, a *app.App, id merchant.ID, limit int) ([]PaymentSettlement, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	return cp.ListPendingPaymentSettlements(ctx, id, limit)
}

// AcknowledgePaymentSettlement marks one of the merchant's events as processed
// by the embedding host.
func AcknowledgePaymentSettlement(ctx context.Context, a *app.App, id merchant.ID, settlementID uuid.UUID) error {
	cp := Get(a)
	if cp == nil {
		return fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	return cp.AcknowledgePaymentSettlement(ctx, id, settlementID)
}
