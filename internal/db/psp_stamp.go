package db

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

type pspIDCtxKey struct{}

// ErrNoPSPInContext refuses a provider-bound write without a resolved PSP:
// psp_id is NOT NULL on every provider-bound table, and an unattributed row
// would be indistinguishable from a sibling account's.
var ErrNoPSPInContext = errors.New("no PSP resolved for this provider operation")

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

// WithCustodianID pins the custodian a write is addressed to. A batch account
// updater upload goes to a custodian that backs many PSPs, so its provenance
// is the custodian, not a gateway account.
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

// RequireCustodianID returns the authenticated or captured custodian account.
func RequireCustodianID(ctx context.Context) (uuid.UUID, error) {
	id := CustodianIDFromContext(ctx)
	if id == uuid.Nil {
		return uuid.Nil, errors.New("no custodian resolved for this provider operation")
	}
	return id, nil
}
