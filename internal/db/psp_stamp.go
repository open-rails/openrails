package db

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

type pspIDCtxKey struct{}

// ErrNoPSPInContext is returned when a provider-bound write reaches the repo
// without a resolved PSP. or#893: `psp_id` is NOT NULL on every provider-bound
// table, so this is a refusal, not a degraded write — an unattributed mirror row
// is invisible to a PSP-scoped prune and indistinguishable from a sibling
// account's row.
var ErrNoPSPInContext = errors.New("no PSP resolved for this provider write")

// WithPSPID pins the external account that actually produced a row.
func WithPSPID(ctx context.Context, id uuid.UUID) context.Context {
	if id == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, pspIDCtxKey{}, id)
}

// PSPIDFromContext returns the pinned PSP, or uuid.Nil when none was pinned.
func PSPIDFromContext(ctx context.Context) uuid.UUID {
	v, ok := ctx.Value(pspIDCtxKey{}).(uuid.UUID)
	if !ok {
		return uuid.Nil
	}
	return v
}

// RequirePSPID returns the PSP that produced this row, or fails. Only explicitly
// observed provenance is ever returned — nothing is invented.
func RequirePSPID(ctx context.Context) (uuid.UUID, error) {
	id := PSPIDFromContext(ctx)
	if id == uuid.Nil {
		return uuid.Nil, ErrNoPSPInContext
	}
	return id, nil
}

type custodianIDCtxKey struct{}

// WithCustodianID pins the custodian a write is addressed to. or#893/or#795:
// the batch account updater uploads one token batch to a custodian that backs
// MANY PSPs, so its provenance is the custodian, not a gateway account.
func WithCustodianID(ctx context.Context, id uuid.UUID) context.Context {
	if id == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, custodianIDCtxKey{}, id)
}

// CustodianIDFromContext returns the pinned custodian, or uuid.Nil.
func CustodianIDFromContext(ctx context.Context) uuid.UUID {
	v, ok := ctx.Value(custodianIDCtxKey{}).(uuid.UUID)
	if !ok {
		return uuid.Nil
	}
	return v
}
