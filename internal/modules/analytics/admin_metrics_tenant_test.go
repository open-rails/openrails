package analytics

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/tenant"
)

// TestTenantFilterScopesByTenant proves the metrics query builder emits a
// tenant predicate bound to the resolved tenant id (issue #232). This is the
// per-tenant admin path: a tenant operator's metrics query is always pinned to
// WHERE tenant_id = ?.
func TestTenantFilterScopesByTenant(t *testing.T) {
	tid := tenant.ID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	filter, args := tenantFilter(tid, false)

	if !strings.Contains(filter, "tenant_id = ?") {
		t.Fatalf("filter = %q, want it to contain a tenant_id predicate", filter)
	}
	if len(args) != 1 {
		t.Fatalf("args = %v, want exactly one bound tenant arg", args)
	}
	if got, ok := args[0].(string); !ok || got != tid.String() {
		t.Fatalf("bound arg = %v, want tenant id %q", args[0], tid.String())
	}
}

// TestTenantFilterDefaultTenant proves single-tenant / self-hosted installs are
// still scoped to the well-known default tenant rather than reading unscoped.
func TestTenantFilterDefaultTenant(t *testing.T) {
	filter, args := tenantFilter(tenant.DefaultID, false)
	if !strings.Contains(filter, "tenant_id = ?") {
		t.Fatalf("filter = %q, want a tenant_id predicate", filter)
	}
	if len(args) != 1 || args[0].(string) != tenant.DefaultID.String() {
		t.Fatalf("args = %v, want default tenant id %q", args, tenant.DefaultID.String())
	}
}

// TestTenantFilterCrossTenantHasNoPredicate proves the platform-superadmin
// cross-tenant path emits NO tenant predicate, so it can read across all
// tenants (issue #232). This path is library-only and must be gated on
// controlplane.PermPlatformSuperadmin by its (future) caller.
func TestTenantFilterCrossTenantHasNoPredicate(t *testing.T) {
	filter, args := tenantFilter(tenant.DefaultID, true)
	if filter != "" {
		t.Fatalf("cross-tenant filter = %q, want empty (no tenant predicate)", filter)
	}
	if len(args) != 0 {
		t.Fatalf("cross-tenant args = %v, want none", args)
	}
}
