package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

type PaymentProviderConfig = merchants.PaymentProviderConfig
type UpsertPaymentProviderConfigRequest = merchants.UpsertPaymentProviderConfigRequest
type PublicPSPConfig = merchants.PublicPSPConfig

// ListPublicCheckoutPSPs returns the exact armed PSPs that a browser checkout
// may offer, including their public browser-driver configuration.
func ListPublicCheckoutPSPs(ctx context.Context, a *app.App, id merchant.ID, environment string) ([]PublicPSPConfig, error) {
	providerService, err := paymentProviderService(a)
	if err != nil {
		return nil, fmt.Errorf("control plane list public checkout psps: %w", err)
	}
	providers, err := providerService.PublicCheckoutPSPs(ctx, id, environment)
	if err != nil {
		return nil, fmt.Errorf("control plane list public checkout psps: %w", err)
	}
	return providers, nil
}

// GetPaymentProviderConfig returns one system-owned merchant PSP
// with credential values redacted.
func GetPaymentProviderConfig(ctx context.Context, a *app.App, id merchant.ID, rail, environment string) (PaymentProviderConfig, error) {
	if strings.TrimSpace(rail) == "" {
		return PaymentProviderConfig{}, errors.New("control plane get payment provider: rail required")
	}
	providerService, err := paymentProviderService(a)
	if err != nil {
		return PaymentProviderConfig{}, fmt.Errorf("control plane get payment provider: %w", err)
	}
	provider, err := providerService.GetPaymentProviderConfig(ctx, id, rail, environment)
	if err != nil {
		return PaymentProviderConfig{}, fmt.Errorf("control plane get payment provider: %w", err)
	}
	return provider, nil
}

// UpsertPaymentProviderConfig configures one PSP for a
// system-owned merchant through the existing merchant-secret backend.
func UpsertPaymentProviderConfig(ctx context.Context, a *app.App, id merchant.ID, rail string, req UpsertPaymentProviderConfigRequest) (PaymentProviderConfig, error) {
	providerService, err := paymentProviderService(a)
	if err != nil {
		return PaymentProviderConfig{}, fmt.Errorf("control plane configure payment provider: %w", err)
	}
	provider, err := providerService.UpsertPaymentProviderConfig(ctx, id, rail, req)
	if err != nil {
		return PaymentProviderConfig{}, fmt.Errorf("control plane configure payment provider: %w", err)
	}
	return provider, nil
}

func paymentProviderService(a *app.App) (*merchants.Service, error) {
	if Get(a) == nil {
		return nil, errors.New("no control plane attached (call Attach first)")
	}
	if a.Runtime == nil {
		return nil, errors.New("runtime unavailable")
	}
	if a.Runtime.Merchants == nil {
		return nil, errors.New("merchant secrets unavailable")
	}
	return a.Runtime.Merchants, nil
}
