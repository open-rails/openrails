// Package tenant provides the tenant-context primitive for OpenRails'
// tenant-aware core data model (issue #223).
//
// OpenRails core is tenant-aware: one shared app/DB serves many tenants, and a
// single-tenant / self-hosted install runs the SAME code paths as its OWN
// explicitly-configured tenant. There is no "default tenant" — every
// tenant-owned DB access must be scoped by a tenant id resolved BEFORE
// authorization and before any tenant-owned query runs, and a missing tenant
// is an error, never a silent fallback (issue #336).
package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ID is a typed tenant / billing-namespace identifier. It is required as an
// explicit parameter on every tenant-owned repository/query; there is no
// implicit global tenant lookup inside repositories.
type ID uuid.UUID

// ErrNoTenant is returned by Require when no tenant has been resolved onto the
// context. It signals a programming/wiring error — every tenant-owned code path
// must resolve a tenant first (HTTP middleware, service-token/JWT resolution,
// background-job enqueue, or the host's configured single tenant).
var ErrNoTenant = errors.New("tenant: no tenant resolved on context")

// UUID returns the underlying uuid.UUID for use in queries.
func (id ID) UUID() uuid.UUID { return uuid.UUID(id) }

// String returns the canonical string form of the tenant id.
func (id ID) String() string { return uuid.UUID(id).String() }

// IsZero reports whether the id is the zero value (unset).
func (id ID) IsZero() bool { return uuid.UUID(id) == uuid.Nil }

// ParseID parses a tenant id from its string form.
func ParseID(s string) (ID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return ID{}, fmt.Errorf("invalid tenant id %q: %w", s, err)
	}
	return ID(u), nil
}

// tenantCtxKey is the unexported context key for the resolved tenant id.
type tenantCtxKey struct{}

// WithID returns a child context carrying the resolved tenant id. Resolution
// plumbing (HTTP middleware, service token/JWT resolution, admin routes, background job
// enqueue) calls this once the tenant is known, before authorization and
// before any tenant-owned DB access.
func WithID(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, id)
}

// FromContext extracts the resolved tenant id from the context. The boolean is
// false when no tenant has been resolved onto the context. Callers that need a
// tenant should use Require, which turns the missing case into an error.
func FromContext(ctx context.Context) (ID, bool) {
	if ctx == nil {
		return ID{}, false
	}
	v := ctx.Value(tenantCtxKey{})
	if v == nil {
		return ID{}, false
	}
	id, ok := v.(ID)
	if !ok || id.IsZero() {
		return ID{}, false
	}
	return id, true
}

// Require returns the resolved tenant id or ErrNoTenant when none is present.
// This is the single accessor for tenant-owned code paths: there is no default
// tenant to fall back to, so a missing tenant surfaces as an error at the call
// site instead of silently attributing the work to the wrong tenant.
func Require(ctx context.Context) (ID, error) {
	if id, ok := FromContext(ctx); ok {
		return id, nil
	}
	return ID{}, ErrNoTenant
}
