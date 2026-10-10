// Package billingauth is OpenRails' auth contract: the host's Auth
// (helpers/auth), the identities the route gate binds, its refusals, and the
// cookie admission every mount shares. It imports no auth provider.
package billingauth

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// UserContext is a signed-in user of the standalone server's own accounts:
// never a billing customer or staff verdict, which the route gate binds
// instead.
type UserContext struct {
	// UserID is the user's UUID.
	UserID        string
	Email         string
	EmailVerified bool
	Username      string
	SessionID     string
	// Merchant is the slug of the merchant the user acts in, when one is
	// active.
	Merchant string
}

// ValidateSubject refuses a UserID that is not a UUID. The returned message
// is client-safe.
func (uc UserContext) ValidateSubject() error {
	if _, err := uuid.Parse(strings.TrimSpace(uc.UserID)); err != nil {
		return fmt.Errorf("subject %q is not a UUID", uc.UserID)
	}
	return nil
}

type userContextCtxKey struct{}

// SetUserContext returns a child context with user context attached.
func SetUserContext(ctx context.Context, uc UserContext) context.Context {
	return context.WithValue(ctx, userContextCtxKey{}, uc)
}

// FromContext extracts user context from a standard context.
func FromContext(ctx context.Context) (UserContext, bool) {
	v := ctx.Value(userContextCtxKey{})
	if v == nil {
		return UserContext{}, false
	}
	uc, ok := v.(UserContext)
	return uc, ok
}
