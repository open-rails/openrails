// Package catalogscope carries verified creator authority inside the engine.
package catalogscope

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/merchant"
)

type Scope struct {
	MerchantID   merchant.ID
	CatalogID    uuid.UUID
	OwnerSubject string
}

type ownerKey struct{}

// ValidateSubject preserves the host's exact identity, without interpreting it
// as a UUID, email, name or case-insensitive identifier.
func ValidateSubject(subject string) error {
	if subject == "" || !utf8.ValidString(subject) || strings.ContainsRune(subject, 0) {
		return fmt.Errorf("catalog owner subject must be nonempty UTF-8 text without NUL")
	}
	return nil
}

func WithOwner(ctx context.Context, scope Scope) (context.Context, error) {
	if err := ValidateSubject(scope.OwnerSubject); err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if scope.MerchantID.IsZero() || scope.MerchantID != mid || scope.CatalogID == uuid.Nil {
		return nil, fmt.Errorf("catalog scope must belong to the authorized merchant")
	}
	return context.WithValue(ctx, ownerKey{}, scope), nil
}

func FromContext(ctx context.Context) (Scope, bool) {
	scope, ok := ctx.Value(ownerKey{}).(Scope)
	return scope, ok
}

// QueryID is nil only for an absent owner scope. A present scope can never
// widen into an unrestricted administrator query, including a zero ID.
func QueryID(ctx context.Context) *uuid.UUID {
	scope, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	id := scope.CatalogID
	return &id
}
