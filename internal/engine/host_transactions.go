package engine

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/pkg/merchant"
)

// The Tx methods run the same commands as the Client inside tx, a
// transaction from the host's pool on this engine's database, for provider
// obligations that must commit atomically with host rows. OpenRails binds the
// engine's merchant to tx and never commits or rolls it back; after an error
// the host rolls back.

// OpenOperationAuthorization reserves capacity; commit it with the host's
// provider obligation before calling the provider.
func (e *Engine) OpenOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.OperationAuthorizationRequest) (*billing.OperationAuthorization, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.OpenOperationAuthorizationTx(ctx, tx, req)
}

// GetOperationAuthorization observes tx's own uncommitted changes.
func (e *Engine) GetOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, operationID string) (*billing.OperationAuthorization, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.GetOperationAuthorizationTx(ctx, tx, operationID)
}

// ReleaseOperationAuthorization commits with the host's proven provider
// non-creation fact. Billing evidence refuses it.
func (e *Engine) ReleaseOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.ReleaseOperationAuthorizationRequest) (*billing.OperationAuthorization, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.ReleaseOperationAuthorizationTx(ctx, tx, req)
}

// RecordProviderBillingObservation appends provider evidence; eligible evidence
// is rated and settled by OpenRails inside tx.
func (e *Engine) RecordProviderBillingObservationTx(ctx context.Context, tx pgx.Tx, req billing.ProviderBillingObservationRequest) (*billing.ProviderBillingQualification, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.RecordProviderBillingObservationTx(ctx, tx, req)
}

func (e *Engine) GetProviderBillingQualificationTx(ctx context.Context, tx pgx.Tx, operationID string) (*billing.ProviderBillingQualification, error) {
	ctx, err := e.bind(ctx)
	if err != nil {
		return nil, err
	}
	return e.svc.GetProviderBillingQualificationTx(ctx, tx, operationID)
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
