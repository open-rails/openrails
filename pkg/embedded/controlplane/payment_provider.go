package controlplane

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

type PaymentProviderConfig = merchants.PaymentProviderConfig
type UpsertPaymentProviderConfigRequest = merchants.UpsertPaymentProviderConfigRequest

// UpsertPaymentProviderConfig configures one provider account for a
// system-owned merchant through the existing merchant-secret backend.
func UpsertPaymentProviderConfig(ctx context.Context, a *app.App, id merchant.ID, rail string, req UpsertPaymentProviderConfigRequest) (PaymentProviderConfig, error) {
	if Get(a) == nil {
		return PaymentProviderConfig{}, fmt.Errorf("control plane configure payment provider: no control plane attached (call Attach first)")
	}
	if a.Runtime == nil {
		return PaymentProviderConfig{}, fmt.Errorf("control plane configure payment provider: runtime unavailable")
	}
	if a.Runtime.Merchants == nil {
		return PaymentProviderConfig{}, fmt.Errorf("control plane configure payment provider: merchant secrets unavailable")
	}
	provider, err := a.Runtime.Merchants.UpsertPaymentProviderConfig(ctx, id, rail, req)
	if err != nil {
		return PaymentProviderConfig{}, fmt.Errorf("control plane configure payment provider: %w", err)
	}
	return provider, nil
}
