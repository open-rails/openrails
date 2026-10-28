package hosttools

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

// Local maintenance over an owned engine graph: process ownership is the
// authority, so none of these is reachable through a Client or HTTP route.

func initialized(a *app.App) error {
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return fmt.Errorf("openrails: engine is not initialized")
	}
	return nil
}

// Converge runs one merchant-wide convergence pass now: the engine the
// scheduled sweep uses, so grants and entitlements derive right after an import.
func Converge(ctx context.Context, a *app.App, merchantID billing.MerchantID) (ConvergeMerchantResult, error) {
	if err := initialized(a); err != nil {
		return ConvergeMerchantResult{}, err
	}
	return ConvergeMerchant(ctx, ConvergeMerchantOptions{
		Config: a.Config, PGXPool: a.Runtime.DB.Pool(), MerchantID: merchantID, Clock: a.Runtime.Clock,
		MerchantConfig: a.Runtime.DB.MerchantConfig(),
	})
}

// ResolveMerchant captures the immutable merchant ID and current name behind a
// public name. Carry the ID, never the name, into later operations.
func ResolveMerchant(ctx context.Context, a *app.App, name string) (billing.MerchantID, string, error) {
	if err := initialized(a); err != nil {
		return billing.MerchantID{}, "", err
	}
	if a.Runtime.Merchants == nil {
		return billing.MerchantID{}, "", fmt.Errorf("openrails: merchant directory is not armed")
	}
	selected, err := a.Runtime.Merchants.GetBySlug(ctx, name)
	if err != nil {
		return billing.MerchantID{}, "", err
	}
	return selected.ID, selected.Slug, nil
}

// RegisterMerchantForRestore registers a preserved merchant UUID for an
// embedding host. Call during startup, before serving or starting workers.
// It registers no PSPs or credentials.
func RegisterMerchantForRestore(ctx context.Context, a *app.App, id billing.MerchantID, slug string) (billing.MerchantID, error) {
	if err := initialized(a); err != nil {
		return billing.MerchantID{}, err
	}
	if a.Standalone {
		return billing.MerchantID{}, fmt.Errorf("openrails: a standalone server restores through ProvisionMerchantForRestore with destination group authority")
	}
	directory, err := merchants.NewDirectoryService(a.Runtime.DB.DataPool())
	if err != nil {
		return billing.MerchantID{}, err
	}
	m, _, err := directory.RegisterForRestore(ctx, id, slug)
	if err != nil {
		return billing.MerchantID{}, err
	}
	return m.ID, nil
}

// ResolveSolanaPayReview closes a Solana Pay review receipt (a second, late,
// short or unreadable transfer, or an overpayment's excess) once its money was
// settled outside OpenRails. Unresolved reviews hold the billing archive back.
func ResolveSolanaPayReview(ctx context.Context, a *app.App, merchantID billing.MerchantID, signature, resolution string) error {
	if err := initialized(a); err != nil {
		return err
	}
	if a.Runtime.OrderAttemptService == nil {
		return fmt.Errorf("openrails: this engine has no order attempts")
	}
	return a.Runtime.OrderAttemptService.ResolveSolanaPayReview(merchant.WithID(ctx, merchantID), signature, resolution)
}
