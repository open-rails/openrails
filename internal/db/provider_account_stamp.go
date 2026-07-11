package db

import (
	"context"

	"github.com/google/uuid"
)

type railMerchantAccountIDCtxKey struct{}

// WithPSPID pins the external account that actually produced a row.
func WithPSPID(ctx context.Context, id uuid.UUID) context.Context {
	if id == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, railMerchantAccountIDCtxKey{}, id)
}

func railMerchantAccountIDFromContext(ctx context.Context) *uuid.UUID {
	v, ok := ctx.Value(railMerchantAccountIDCtxKey{}).(uuid.UUID)
	if !ok || v == uuid.Nil {
		return nil
	}
	return &v
}

// ResolveRailMerchantAccountIDForStamp returns only explicitly observed provenance.
// ponytail: nil is better than inventing provider-account provenance.
func ResolveRailMerchantAccountIDForStamp(ctx context.Context) *uuid.UUID {
	return railMerchantAccountIDFromContext(ctx)
}
