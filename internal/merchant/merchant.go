// Package merchant carries the resolved merchant through a request. A
// merchant is a billing bucket ("whose books?"), never an identity or an
// authority. There is no default merchant: every merchant-owned query needs a
// merchant resolved first, and a missing one is an error, never a fallback.
package merchant

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/billing"
)

// BoundHeader is the response header naming the merchant a merchant-bound
// request acted on (`OpenRails-Bound-Merchant: <uuid>`): the one its
// credential is bound to. The Go client checks it against its handle's
// merchant.
const BoundHeader = "OpenRails-Bound-Merchant"

// ExpectedHeader is an optional request precondition: the authenticated
// credential must be bound to this merchant. It never selects a merchant
// or grants authority, and a mismatch is refused before any operation runs.
const ExpectedHeader = "OpenRails-Expected-Merchant"

// ErrNoMerchant is returned by Require when no merchant has been resolved onto
// the context. It signals a programming/wiring error — every merchant-owned code
// path must resolve a merchant first (the route gate, a handle, or a
// background job's own).
var ErrNoMerchant = errors.New("merchant: no merchant resolved on context")

// merchantCtxKey is the unexported context key for the resolved merchant id.
type merchantCtxKey struct{}

// WithID returns a child context carrying the resolved merchant id. Resolution
// plumbing (the route gate, a handle's call, background job enqueue) calls
// this once the merchant is known, before any merchant-owned DB access.
func WithID(ctx context.Context, id billing.MerchantID) context.Context {
	return context.WithValue(ctx, merchantCtxKey{}, id)
}

// FromContext extracts the resolved merchant id from the context. The boolean is
// false when no merchant has been resolved onto the context. Callers that need a
// merchant should use Require, which turns the missing case into an error.
func FromContext(ctx context.Context) (billing.MerchantID, bool) {
	if ctx == nil {
		return billing.MerchantID{}, false
	}
	id, ok := ctx.Value(merchantCtxKey{}).(billing.MerchantID)
	if !ok || id.IsZero() {
		return billing.MerchantID{}, false
	}
	return id, true
}

// Require returns the resolved merchant id, or ErrNoMerchant: there is no
// default merchant to fall back to.
func Require(ctx context.Context) (billing.MerchantID, error) {
	if id, ok := FromContext(ctx); ok {
		return id, nil
	}
	return billing.MerchantID{}, ErrNoMerchant
}
