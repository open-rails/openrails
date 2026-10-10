package engine

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// The Tx methods run the same commands as the Client inside tx, a
// transaction from the host's pool on this engine's database, for provider
// operations that must commit atomically with host rows. OpenRails binds the
// engine's merchant to tx and never commits or rolls it back; after an error
// the host rolls back.

// OpenProviderOperationTx reserves capacity; commit it with the host's
// provider obligation before calling the provider.
func (e *Engine) OpenProviderOperationTx(ctx context.Context, tx pgx.Tx, req billing.OpenProviderOperationParams) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.OpenProviderOperationTx(ctx, tx, req)
}

// GetProviderOperationTx observes tx's own uncommitted changes.
func (e *Engine) GetProviderOperationTx(ctx context.Context, tx pgx.Tx, operationID string) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.GetProviderOperationTx(ctx, tx, operationID)
}

// IncrementProviderOperationTx grows an open hold; a refusal writes nothing
// to tx.
func (e *Engine) IncrementProviderOperationTx(ctx context.Context, tx pgx.Tx, req billing.IncrementProviderOperationParams) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.IncrementProviderOperationTx(ctx, tx, req)
}

// ReleaseProviderOperationTx commits with the host's proven provider
// non-creation fact. Billing evidence refuses it.
func (e *Engine) ReleaseProviderOperationTx(ctx context.Context, tx pgx.Tx, req billing.ReleaseProviderOperationParams) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.ReleaseProviderOperationTx(ctx, tx, req)
}

// RecordProviderBillingObservationTx appends provider evidence, or the host's
// refusal to produce it; eligible evidence is rated and settled inside tx.
func (e *Engine) RecordProviderBillingObservationTx(ctx context.Context, tx pgx.Tx, req billing.RecordProviderBillingObservationParams) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.RecordProviderBillingObservationTx(ctx, tx, req)
}

// CloseProviderOperationTx closes a refused hold on an operator's attestation
// inside tx.
func (e *Engine) CloseProviderOperationTx(ctx context.Context, tx pgx.Tx, req billing.CloseProviderOperationParams) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.CloseProviderOperationTx(ctx, tx, req)
}

// bind applies the engine's merchant exactly as the in-process Client does.
func (e *Engine) bind(ctx context.Context) (context.Context, error) {
	bound := e.App.Runtime.ConfiguredMerchant()
	if bound.IsZero() {
		return ctx, fmt.Errorf("openrails: no merchant is bound; declare Config.Merchant")
	}
	if pinned, ok := merchant.FromContext(ctx); ok && pinned != bound {
		return ctx, fmt.Errorf("%w: %s", billing.ErrConflict, merchantMismatchMsg(bound, pinned))
	}
	return merchant.WithID(ctx, bound), nil
}
