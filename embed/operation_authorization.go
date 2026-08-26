package embed

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/service"
)

type OperationAuthorizationRequest = service.OperationAuthorizationRequest
type OperationAuthorization = service.OperationAuthorization
type OperationAuthorizationState = service.OperationAuthorizationState
type ReleaseOperationAuthorizationRequest = service.ReleaseOperationAuthorizationRequest
type OperationAuthorizationConflict = service.OperationAuthorizationConflict

const (
	OperationAuthorizationOpen     = service.OperationAuthorizationOpen
	OperationAuthorizationReleased = service.OperationAuthorizationReleased
	OperationAuthorizationSettled  = service.OperationAuthorizationSettled
)

var (
	ErrOperationAuthorizationConflict = service.ErrOperationAuthorizationConflict
	ErrOperationAuthorizationNotFound = service.ErrOperationAuthorizationNotFound
	ErrOperationAuthorizationNotOpen  = service.ErrOperationAuthorizationNotOpen
)

// OpenOperationAuthorizationTx reserves account capacity inside a transaction
// owned by the embedding host. The host can add its provider obligation to tx
// and commit both facts atomically; OpenRails never commits or rolls tx back.
func (r *Runtime) OpenOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req OperationAuthorizationRequest) (*OperationAuthorization, error) {
	ctx, err := r.operationAuthorizationContext(ctx)
	if err != nil {
		return nil, err
	}
	return r.svc.OpenOperationAuthorizationTx(ctx, tx, req)
}

// GetOperationAuthorization reads the bound merchant's durable authorization.
func (r *Runtime) GetOperationAuthorization(ctx context.Context, operationID string) (*OperationAuthorization, error) {
	ctx, err := r.operationAuthorizationContext(ctx)
	if err != nil {
		return nil, err
	}
	return r.svc.GetOperationAuthorization(ctx, operationID)
}

// ReleaseOperationAuthorization releases capacity after the host proves the
// provider create did not occur. Provider readback semantics stay host-owned;
// OpenRails only binds the opaque release reference.
func (r *Runtime) ReleaseOperationAuthorization(ctx context.Context, req ReleaseOperationAuthorizationRequest) (*OperationAuthorization, error) {
	ctx, err := r.operationAuthorizationContext(ctx)
	if err != nil {
		return nil, err
	}
	return r.svc.ReleaseOperationAuthorization(ctx, req)
}

func (r *Runtime) operationAuthorizationContext(ctx context.Context) (context.Context, error) {
	if r == nil || r.emb == nil || r.svc == nil {
		return ctx, fmt.Errorf("openrails embed: runtime is not initialized")
	}
	bound := r.emb.App().Runtime.ConfiguredMerchant()
	if bound.IsZero() {
		return ctx, fmt.Errorf("openrails embed: no merchant is bound")
	}
	if pinned, ok := merchant.FromContext(ctx); ok && pinned != bound {
		return ctx, fmt.Errorf("%s", merchantMismatchMsg(bound, pinned))
	}
	return merchant.WithID(ctx, bound), nil
}
