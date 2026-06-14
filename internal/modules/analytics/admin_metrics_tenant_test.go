package analytics

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestTenantFilterScopesByTenant proves the metrics query builder emits a
// tenant predicate bound to the resolved tenant id (issue #232). This is the
// per-tenant admin path: a tenant operator's metrics query is always pinned to
// WHERE tenant_id = ?.
func TestTenantFilterScopesByTenant(t *testing.T) {
	tid := merchant.ID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
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

// TestTenantFilterScopedTenant proves a resolved tenant id is always scoped
// to WHERE tenant_id = ? rather than reading unscoped.
func TestTenantFilterScopedTenant(t *testing.T) {
	filter, args := tenantFilter(dbtest.TestTenantID, false)
	if !strings.Contains(filter, "tenant_id = ?") {
		t.Fatalf("filter = %q, want a tenant_id predicate", filter)
	}
	if len(args) != 1 || args[0].(string) != dbtest.TestTenantID.String() {
		t.Fatalf("args = %v, want tenant id %q", args, dbtest.TestTenantID.String())
	}
}

// TestTenantFilterCrossTenantHasNoPredicate proves the platform-superadmin
// cross-tenant path emits NO tenant predicate, so it can read across all
// tenants (issue #232). This path is library-only and must be gated on
// controlplane.PermPlatformSuperadmin by its (future) caller.
func TestTenantFilterCrossTenantHasNoPredicate(t *testing.T) {
	filter, args := tenantFilter(dbtest.TestTenantID, true)
	if filter != "" {
		t.Fatalf("cross-tenant filter = %q, want empty (no tenant predicate)", filter)
	}
	if len(args) != 0 {
		t.Fatalf("cross-tenant args = %v, want none", args)
	}
}
