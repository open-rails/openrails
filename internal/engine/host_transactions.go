package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

// The Tx methods run the same commands as the Client inside tx, a
// transaction from the host's pool on this engine's database, for provider
// operations and running charges that must commit atomically with host rows.
// OpenRails binds mid, the handle's merchant, to tx and never commits or rolls
// it back; after an error the host rolls back.

// OpenProviderOperationTx reserves capacity; commit it with the host's
// provider obligation before calling the provider.
func (e *Engine) OpenProviderOperationTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, req billing.OpenProviderOperationParams) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	return e.svc.OpenProviderOperationTx(ctx, tx, req)
}

// GetProviderOperationTx observes tx's own uncommitted changes.
func (e *Engine) GetProviderOperationTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, operationID string) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	return e.svc.GetProviderOperationTx(ctx, tx, operationID)
}

// IncrementProviderOperationTx grows an open hold; a refusal writes nothing
// to tx.
func (e *Engine) IncrementProviderOperationTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, req billing.IncrementProviderOperationParams) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	return e.svc.IncrementProviderOperationTx(ctx, tx, req)
}

// ReleaseProviderOperationTx commits with the host's proven provider
// non-creation fact. Billing evidence refuses it.
func (e *Engine) ReleaseProviderOperationTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, req billing.ReleaseProviderOperationParams) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	return e.svc.ReleaseProviderOperationTx(ctx, tx, req)
}

// RecordProviderBillingObservationTx appends provider evidence, or the host's
// refusal to produce it; eligible evidence is rated and settled inside tx.
func (e *Engine) RecordProviderBillingObservationTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, req billing.RecordProviderBillingObservationParams) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	return e.svc.RecordProviderBillingObservationTx(ctx, tx, req)
}

// CloseProviderOperationTx closes a refused hold on an operator's attestation
// inside tx.
func (e *Engine) CloseProviderOperationTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, req billing.CloseProviderOperationParams) (*billing.ProviderOperation, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	return e.svc.CloseProviderOperationTx(ctx, tx, req)
}

// StartRunningChargeTx starts a charge, or answers the one its reference
// names; commit it with the host's fact that the work is ready.
func (e *Engine) StartRunningChargeTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, params billing.StartRunningChargeParams) (*billing.RunningCharge, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	charge, _, err := e.svc.StartRunningChargeTx(ctx, tx, params)
	return charge, err
}

// StopRunningChargeTx stops a charge and posts the rest of it; commit it with
// the host's fact that the work was released.
func (e *Engine) StopRunningChargeTx(ctx context.Context, mid billing.MerchantID, tx pgx.Tx, reference string, params billing.StopRunningChargeParams) (*billing.RunningCharge, error) {
	ctx, err := e.bind(ctx, mid, tx)
	if err != nil {
		return nil, err
	}
	return e.svc.StopRunningChargeTx(ctx, tx, reference, params)
}

// bind pins mid for a call inside tx, after reading it live through tx: an
// unknown, deleted or archived merchant is billing.ErrMerchantUnresolved.
func (e *Engine) bind(ctx context.Context, mid billing.MerchantID, tx pgx.Tx) (context.Context, error) {
	if tx == nil {
		return ctx, fmt.Errorf("openrails: a transaction is required")
	}
	row, err := e.App.Runtime.DB.NewWithPgxTx(tx).Gen(ctx).GetMerchantDirectoryByID(ctx, mid.UUID())
	switch {
	case errors.Is(err, pgx.ErrNoRows), err == nil && row.Status != string(merchants.StatusActive):
		return ctx, fmt.Errorf("%w: %s is not a live merchant of this engine", billing.ErrMerchantUnresolved, mid)
	case err != nil:
		return ctx, fmt.Errorf("openrails: read merchant %s: %w", mid, err)
	}
	return merchant.WithID(ctx, mid), nil
}
