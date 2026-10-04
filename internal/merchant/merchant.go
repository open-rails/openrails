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
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// SelectorHeader names the merchant a request targets: a public slug
// (`OpenRails-Merchant: shop`) or an immutable id (`OpenRails-Merchant:
// id:<uuid>`). It grants no authority: the server resolves it, the route
// authorizes the credential for that merchant, and only then is it pinned.
const SelectorHeader = "OpenRails-Merchant"

// selectorIDPrefix marks the id form of a selector; a slug never contains ":".
const selectorIDPrefix = "id:"

// Selector is a parsed SelectorHeader: exactly one of Slug and ID is set.
type Selector struct {
	Slug string
	ID   billing.MerchantID
}

// ErrSelector is a SelectorHeader that is repeated, blank or malformed.
var ErrSelector = errors.New("merchant: invalid OpenRails-Merchant selector")

// ParseSelector reads the request's selector. present is false when the
// header is absent; a present header that is repeated, blank or malformed is
// ErrSelector.
func ParseSelector(h http.Header) (selector Selector, present bool, err error) {
	values, present := h[http.CanonicalHeaderKey(SelectorHeader)]
	if !present {
		return Selector{}, false, nil
	}
	if len(values) != 1 {
		return Selector{}, true, ErrSelector
	}
	raw := strings.TrimSpace(values[0])
	if id, ok := strings.CutPrefix(raw, selectorIDPrefix); ok {
		parsed, err := billing.ParseMerchantID(strings.TrimSpace(id))
		if err != nil || parsed.IsZero() {
			return Selector{}, true, ErrSelector
		}
		return Selector{ID: parsed}, true, nil
	}
	if raw == "" || billing.ValidateMerchantSlug(raw) != nil {
		return Selector{}, true, ErrSelector
	}
	return Selector{Slug: billing.NormalizeMerchantSlug(raw)}, true, nil
}

// String is the selector's header value.
func (s Selector) String() string {
	if !s.ID.IsZero() {
		return selectorIDPrefix + s.ID.String()
	}
	return s.Slug
}

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
