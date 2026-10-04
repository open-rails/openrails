// Package merchant carries the resolved merchant through a request (#480).
//
// A merchant is a dumb billing bucket — it answers "whose books does this row go
// on?", never "who are you / what may you do" (auth is AuthKit's job). One shared
// app/DB serves many merchants, and a single-merchant / self-hosted install runs
// the SAME code paths as its OWN explicitly-registered merchant. There is no
// "default merchant" — every merchant-owned DB access must be scoped by a
// merchant id resolved BEFORE any merchant-owned query runs, and a missing
// merchant is an error, never a silent fallback.
package merchant

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/billing"
)

// BindingHeader asserts an expected immutable merchant UUID. It grants no
// authority; merchant routes compare it with the authenticated principal before
// acquiring a merchant database connection.
const BindingHeader = "X-OpenRails-Merchant-ID"

// SlugHeader selects a merchant by public slug. Resolution and authorization
// happen on the server; the selector grants no authority. It must not be
// combined with BindingHeader on one request.
const SlugHeader = "X-OpenRails-Merchant-Slug"

// ErrNoMerchant is returned by Require when no merchant has been resolved onto
// the context. It signals a programming/wiring error — every merchant-owned code
// path must resolve a merchant first (HTTP middleware, host authenticator,
// background-job enqueue, or the host's configured merchant).
var ErrNoMerchant = errors.New("merchant: no merchant resolved on context")

// merchantCtxKey is the unexported context key for the resolved merchant id.
type merchantCtxKey struct{}

// WithID returns a child context carrying the resolved merchant id. Resolution
// plumbing (HTTP middleware, host authenticator, admin routes, background job
// enqueue) calls this once the merchant is known, before any merchant-owned DB
// access.
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

// Require returns the resolved merchant id or ErrNoMerchant when none is present.
// This is the single accessor for merchant-owned code paths: there is no default
// merchant to fall back to, so a missing merchant surfaces as an error at the
// call site instead of silently attributing the work to the wrong merchant.
func Require(ctx context.Context) (billing.MerchantID, error) {
	if id, ok := FromContext(ctx); ok {
		return id, nil
	}
	return billing.MerchantID{}, ErrNoMerchant
}
