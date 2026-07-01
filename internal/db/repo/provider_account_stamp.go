package repo

import (
	"context"

	"github.com/google/uuid"
)

type providerAccountIDCtxKey struct{}

// WithProviderAccountID pins the external account that actually produced a row.
func WithProviderAccountID(ctx context.Context, id uuid.UUID) context.Context {
	if id == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, providerAccountIDCtxKey{}, id)
}

func providerAccountIDFromContext(ctx context.Context) *uuid.UUID {
	v, ok := ctx.Value(providerAccountIDCtxKey{}).(uuid.UUID)
	if !ok || v == uuid.Nil {
		return nil
	}
	return &v
}

// resolveProviderAccountIDForStamp returns only explicitly observed provenance.
// ponytail: nil is better than inventing provider-account provenance.
func resolveProviderAccountIDForStamp(ctx context.Context) *uuid.UUID {
	return providerAccountIDFromContext(ctx)
}
