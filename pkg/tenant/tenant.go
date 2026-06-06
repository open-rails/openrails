// Package tenant provides the tenant-context primitive for OpenRails'
// tenant-aware core data model (issue #223).
//
// OpenRails core is tenant-aware so that one shared app/DB can serve many
// tenants, while self-hosted single-tenant installs run the SAME code paths
// with one default tenant namespace. Every tenant-owned DB access must be
// scoped by a tenant id resolved BEFORE authorization and before any
// tenant-owned query runs.
//
// The single-tenant / self-hosted case is modelled explicitly as the
// well-known Default tenant (DefaultID / "default" slug, seeded by migration
// 039). Code that does not (yet) resolve a tenant from the request defaults to
// the Default tenant via FromContextOrDefault, so the same query layer serves
// both single-tenant and multi-tenant deployments without branching.
package tenant

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ID is a typed tenant / billing-namespace identifier. It is required as an
// explicit parameter on every tenant-owned repository/query; there is no
// implicit global tenant lookup inside repositories.
type ID uuid.UUID

// DefaultID is the well-known id of the single default tenant seeded by
// migration 039 (slug "default"). Self-hosted single-tenant installs operate
// entirely as this tenant.
//
// It is deterministic (00000000-0000-0000-0000-000000000001) so application
// code, tests, and external services can rely on it.
var DefaultID = ID(uuid.MustParse("00000000-0000-0000-0000-000000000001"))

// DefaultSlug is the stable slug of the single default tenant.
const DefaultSlug = "default"

// UUID returns the underlying uuid.UUID for use in queries.
func (id ID) UUID() uuid.UUID { return uuid.UUID(id) }

// String returns the canonical string form of the tenant id.
func (id ID) String() string { return uuid.UUID(id).String() }

// IsZero reports whether the id is the zero value (unset).
func (id ID) IsZero() bool { return uuid.UUID(id) == uuid.Nil }

// IsDefault reports whether this id is the well-known default tenant.
func (id ID) IsDefault() bool { return id == DefaultID }

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
// false when no tenant has been resolved onto the context (in which case
// callers that should default to single-tenant behaviour use
// FromContextOrDefault instead).
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

// FromContextOrDefault returns the resolved tenant id, or the Default tenant
// when none has been resolved. This is the single-tenant / self-hosted code
// path: callers that have not (yet) been wired to explicit per-request tenant
// resolution still run correctly as the one default tenant namespace.
//
// Once full multi-tenant resolution is enforced on every entry point, callers
// can switch to FromContext + a hard failure on the missing case. Keeping the
// default here is what lets single-tenant and multi-tenant share one path.
func FromContextOrDefault(ctx context.Context) ID {
	if id, ok := FromContext(ctx); ok {
		return id
	}
	return DefaultID
}
