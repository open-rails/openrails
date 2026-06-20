// Package merchant provides the merchant-context primitive for OpenRails'
// merchant-scoped billing data model (#480).
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
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// slugRe is the legal merchant-slug pattern. It is IDENTICAL to AuthKit's
// org-slug rule (authkit/core validateOrgSlug: `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`
// — lowercase alnum + hyphens, no leading/trailing hyphen, ≤63 chars) so that a
// merchant slug is ALWAYS a legal backing-org slug. The merchant.slug ==
// backing-org.slug invariant (#548) requires this: a merchant must be able to
// own a same-slug AuthKit org in standalone mode.
var slugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeSlug returns the canonical form of a merchant slug (trimmed,
// lowercased). The canonical form is what is stored and what must match the
// backing org's slug.
func NormalizeSlug(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidateSlug reports whether the NORMALIZED form of s is a legal merchant slug
// — equivalently, a legal AuthKit org slug (#548). Callers should normalize
// (NormalizeSlug) and store/use that form so merchant.slug == backing-org.slug
// holds by construction.
func ValidateSlug(s string) error {
	n := NormalizeSlug(s)
	if !slugRe.MatchString(n) {
		return fmt.Errorf("invalid merchant slug %q: must be a legal AuthKit org slug (lowercase a-z0-9 and hyphens, no leading/trailing hyphen, ≤63 chars)", s)
	}
	return nil
}

// ID is a typed merchant / billing-namespace identifier. It is required as an
// explicit parameter on every merchant-owned repository/query; there is no
// implicit global merchant lookup inside repositories. Canonical merchant uuid =
// openrails.merchants.id (self-owned uuidv7, never an AuthKit uuid).
type ID uuid.UUID

// ErrNoMerchant is returned by Require when no merchant has been resolved onto
// the context. It signals a programming/wiring error — every merchant-owned code
// path must resolve a merchant first (HTTP middleware, host authenticator,
// background-job enqueue, or the host's configured merchant).
var ErrNoMerchant = errors.New("merchant: no merchant resolved on context")

// UUID returns the underlying uuid.UUID for use in queries.
func (id ID) UUID() uuid.UUID { return uuid.UUID(id) }

// String returns the canonical string form of the merchant id.
func (id ID) String() string { return uuid.UUID(id).String() }

// IsZero reports whether the id is the zero value (unset).
func (id ID) IsZero() bool { return uuid.UUID(id) == uuid.Nil }

// ParseID parses a merchant id from its string form.
func ParseID(s string) (ID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return ID{}, fmt.Errorf("invalid merchant id %q: %w", s, err)
	}
	return ID(u), nil
}

// merchantCtxKey is the unexported context key for the resolved merchant id.
type merchantCtxKey struct{}

// WithID returns a child context carrying the resolved merchant id. Resolution
// plumbing (HTTP middleware, host authenticator, admin routes, background job
// enqueue) calls this once the merchant is known, before any merchant-owned DB
// access.
func WithID(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, merchantCtxKey{}, id)
}

// FromContext extracts the resolved merchant id from the context. The boolean is
// false when no merchant has been resolved onto the context. Callers that need a
// merchant should use Require, which turns the missing case into an error.
func FromContext(ctx context.Context) (ID, bool) {
	if ctx == nil {
		return ID{}, false
	}
	v := ctx.Value(merchantCtxKey{})
	if v == nil {
		return ID{}, false
	}
	id, ok := v.(ID)
	if !ok || id.IsZero() {
		return ID{}, false
	}
	return id, true
}

// Require returns the resolved merchant id or ErrNoMerchant when none is present.
// This is the single accessor for merchant-owned code paths: there is no default
// merchant to fall back to, so a missing merchant surfaces as an error at the
// call site instead of silently attributing the work to the wrong merchant.
func Require(ctx context.Context) (ID, error) {
	if id, ok := FromContext(ctx); ok {
		return id, nil
	}
	return ID{}, ErrNoMerchant
}
