package controlplane

import "testing"

// TestOperatorRoleExcludesPlatformSuperadmin proves the #226 separation: a
// tenant operator role is granted the full catalog EXCEPT the cross-tenant
// platform-superadmin permission, so a tenant operator admin can never reach the
// platform surface via their operator org.
func TestOperatorRoleExcludesPlatformSuperadmin(t *testing.T) {
	for _, p := range OperatorRolePermissions() {
		if p == PermPlatformSuperadmin {
			t.Fatalf("operator role must NOT include %q", PermPlatformSuperadmin)
		}
	}
	// Operator role still has the broad per-tenant admin permission.
	if !contains(OperatorRolePermissions(), PermAdmin) {
		t.Fatalf("operator role must include %q", PermAdmin)
	}
}

// TestPlatformRoleHoldsOnlySuperadmin proves the platform role is narrow: it
// holds ONLY the platform-superadmin permission.
func TestPlatformRoleHoldsOnlySuperadmin(t *testing.T) {
	perms := PlatformRolePermissions()
	if len(perms) != 1 || perms[0] != PermPlatformSuperadmin {
		t.Fatalf("platform role must hold only %q, got %v", PermPlatformSuperadmin, perms)
	}
}

// TestSuperadminPermInCatalog proves the permission is a real catalog entry (so
// AuthKit accepts grants of it) and is NOT a self/browser permission.
func TestSuperadminPermInCatalog(t *testing.T) {
	if !contains(CatalogNames(), PermPlatformSuperadmin) {
		t.Fatalf("catalog must include %q", PermPlatformSuperadmin)
	}
	if IsSelfPermission(PermPlatformSuperadmin) {
		t.Fatalf("%q must never be a self/browser permission", PermPlatformSuperadmin)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
