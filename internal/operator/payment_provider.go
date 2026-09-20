package operator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/providerqualification"
	"github.com/open-rails/openrails/pkg/merchant"
)

type PaymentProviderConfig = merchants.PaymentProviderConfig
type UpsertPaymentProviderConfigRequest = merchants.UpsertPaymentProviderConfigRequest
type ArchivePaymentProviderAccountRequest = merchants.ArchivePaymentProviderAccountRequest

// ListPaymentProviderConfigs returns the merchant's PSP accounts on rail (""
// = every rail) filtered by status ("active", "archived", "" = all), redacted.
func ListPaymentProviderConfigs(ctx context.Context, a *app.App, id merchant.ID, rail, status string) ([]PaymentProviderConfig, error) {
	providerService, err := paymentProviderService(a)
	if err != nil {
		return nil, fmt.Errorf("control plane list payment providers: %w", err)
	}
	items, err := providerService.ListPaymentProviderConfigs(ctx, id, rail, "", status)
	if err != nil {
		return nil, fmt.Errorf("control plane list payment providers: %w", err)
	}
	return items, nil
}

// ArchivePaymentProviderAccount archives exactly one PSP account by its
// immutable id without contacting the provider (#655/#656).
func ArchivePaymentProviderAccount(ctx context.Context, a *app.App, id merchant.ID, rail string, pspID uuid.UUID, req ArchivePaymentProviderAccountRequest) (PaymentProviderConfig, error) {
	if strings.TrimSpace(rail) == "" {
		return PaymentProviderConfig{}, errors.New("control plane archive payment provider account: rail required")
	}
	providerService, err := paymentProviderService(a)
	if err != nil {
		return PaymentProviderConfig{}, fmt.Errorf("control plane archive payment provider account: %w", err)
	}
	provider, err := providerService.ArchivePaymentProviderAccount(ctx, id, rail, pspID, req)
	if err != nil {
		return PaymentProviderConfig{}, fmt.Errorf("control plane archive payment provider account: %w", err)
	}
	return provider, nil
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

// SetProviderCutoverQualification installs or revokes only the private operator
// qualification record for an existing merchant-owned account. No provider call.
func SetProviderCutoverQualification(ctx context.Context, a *app.App, id merchant.ID, pspID uuid.UUID, record *providerqualification.Record) error {
	if Get(a) == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return errors.New("control plane runtime is unavailable")
	}
	if id.IsZero() {
		return providerqualification.ErrInvalid
	}
	return a.Runtime.DB.RunInMerchantConn(merchant.WithID(ctx, id), func(ctx context.Context) error {
		return providerqualification.Set(ctx, a.Runtime.DB, pspID, record)
	})
}
