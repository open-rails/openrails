package embed

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/pkg/merchant"
)

// HostTransactions is the embedding-only extension for provider obligations
// that must commit atomically with host rows. Each method runs the same command
// as the shared Client, with the same types and error classes, inside tx: a
// transaction from the host's pool on this runtime's database. OpenRails binds
// the runtime's merchant to tx and never commits or rolls it back. After any
// error the host must roll back.
//
// Ordinary billing code uses Runtime.Client, which works in every deployment.
// A remote Client cannot join a host transaction, and a compensating outbox is
// not a substitute for this atomicity.
type HostTransactions struct{ rt *Runtime }

// HostTransactions returns the host transaction extension for this runtime.
func (r *Runtime) HostTransactions() *HostTransactions { return &HostTransactions{rt: r} }

// OpenOperationAuthorization reserves capacity; commit it with the host's
// provider obligation before calling the provider.
func (h *HostTransactions) OpenOperationAuthorization(ctx context.Context, tx pgx.Tx, req openrails.OperationAuthorizationRequest) (*openrails.OperationAuthorization, error) {
	ctx, err := h.bind(ctx)
	if err != nil {
		return nil, err
	}
	return h.rt.svc.OpenOperationAuthorizationTx(ctx, tx, req)
}

// GetOperationAuthorization observes tx's own uncommitted changes.
func (h *HostTransactions) GetOperationAuthorization(ctx context.Context, tx pgx.Tx, operationID string) (*openrails.OperationAuthorization, error) {
	ctx, err := h.bind(ctx)
	if err != nil {
		return nil, err
	}
	return h.rt.svc.GetOperationAuthorizationTx(ctx, tx, operationID)
}

// ReleaseOperationAuthorization commits with the host's proven provider
// non-creation fact. Billing evidence refuses it.
func (h *HostTransactions) ReleaseOperationAuthorization(ctx context.Context, tx pgx.Tx, req openrails.ReleaseOperationAuthorizationRequest) (*openrails.OperationAuthorization, error) {
	ctx, err := h.bind(ctx)
	if err != nil {
		return nil, err
	}
	return h.rt.svc.ReleaseOperationAuthorizationTx(ctx, tx, req)
}

// RecordProviderBillingObservation appends provider evidence; eligible evidence
// is rated and settled by OpenRails inside tx.
func (h *HostTransactions) RecordProviderBillingObservation(ctx context.Context, tx pgx.Tx, req openrails.ProviderBillingObservationRequest) (*openrails.ProviderBillingQualification, error) {
	ctx, err := h.bind(ctx)
	if err != nil {
		return nil, err
	}
	return h.rt.svc.RecordProviderBillingObservationTx(ctx, tx, req)
}

func (h *HostTransactions) GetProviderBillingQualification(ctx context.Context, tx pgx.Tx, operationID string) (*openrails.ProviderBillingQualification, error) {
	ctx, err := h.bind(ctx)
	if err != nil {
		return nil, err
	}
	return h.rt.svc.GetProviderBillingQualificationTx(ctx, tx, operationID)
}

// bind applies the runtime's merchant exactly as the in-process Client does.
func (h *HostTransactions) bind(ctx context.Context) (context.Context, error) {
	if h == nil || h.rt == nil || h.rt.app == nil || h.rt.svc == nil {
		return ctx, fmt.Errorf("openrails embed: runtime is not initialized")
	}
	bound := h.rt.app.Runtime.ConfiguredMerchant()
	if bound.IsZero() {
		return ctx, fmt.Errorf("openrails embed: no merchant is bound")
	}
	if pinned, ok := merchant.FromContext(ctx); ok && pinned != bound {
		return ctx, fmt.Errorf("%w: %s", openrails.ErrConflict, merchantMismatchMsg(bound, pinned))
	}
	return merchant.WithID(ctx, bound), nil
}
