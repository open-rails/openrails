package controlplane

import (
	"testing"
	"time"

	authcore "github.com/open-rails/authkit/core"
)

func TestCatalog_ContainsRequiredPermissions(t *testing.T) {
	// Issue #224 fixed permission catalog. If this list changes, the seeding and
	// the operator-role grant must be reviewed together.
	want := []string{
		"openrails:admin",
		"openrails:credits:read",
		"openrails:credits:write",
		// billing:spend (payer) hot-path capability (#246).
		"openrails:credits:spend",
		"openrails:entitlements:read",
		"openrails:catalog:write",
		"openrails:payments:refund",
		"openrails:subscriptions:cancel",
		// Self-service (browser-direct) permissions (#222 browser tier).
		"openrails:self:billing:read",
		"openrails:self:checkout:create",
		"openrails:self:subscriptions:cancel",
		"openrails:self:payment-methods:manage",
		// Browser-tier delegated-token MINT capability (server-to-server, #222).
		"openrails:self:mint",
		// Cross-tenant managed-hosting platform superadmin (#226). It is in the
		// catalog (a valid grantable permission) but is NOT seeded into operator
		// orgs — see TestOperatorRolePermissions_ExcludesPlatformSuperadmin.
		"openrails:platform:superadmin",
	}
	got := map[string]bool{}
	for _, p := range Catalog() {
		if p.Name == "" {
			t.Fatalf("catalog entry with empty name: %+v", p)
		}
		if p.Description == "" {
			t.Errorf("catalog entry %q missing description", p.Name)
		}
		got[p.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("catalog missing required permission %q", w)
		}
	}
	if len(Catalog()) != len(want) {
		t.Errorf("catalog size = %d, want %d (%v)", len(Catalog()), len(want), CatalogNames())
	}
}

func TestCatalog_IsACopy(t *testing.T) {
	a := Catalog()
	if len(a) == 0 {
		t.Fatal("empty catalog")
	}
	a[0].Name = "mutated"
	if Catalog()[0].Name == "mutated" {
		t.Fatal("Catalog() returned a shared slice; mutation leaked into the package catalog")
	}
}

// TestOperatorRolePermissions_ExcludesPlatformSuperadmin proves the #226
// separation: a tenant operator role holds the FULL catalog EXCEPT the
// cross-tenant platform-superadmin permission (which is seeded only to the
// platform role). Every other catalog permission must be present.
func TestOperatorRolePermissions_ExcludesPlatformSuperadmin(t *testing.T) {
	perms := OperatorRolePermissions()
	names := CatalogNames()
	if len(perms) != len(names)-1 {
		t.Fatalf("operator role perms = %d, want catalog-1 = %d", len(perms), len(names)-1)
	}
	set := map[string]bool{}
	for _, p := range perms {
		set[p] = true
	}
	if set[PermPlatformSuperadmin] {
		t.Fatalf("operator role must NOT include %q", PermPlatformSuperadmin)
	}
	for _, n := range names {
		if n == PermPlatformSuperadmin {
			continue
		}
		if !set[n] {
			t.Errorf("operator role missing catalog permission %q", n)
		}
	}
}

func TestCatalogNames_StableOrder(t *testing.T) {
	// Deterministic order keeps seeding reproducible across runs.
	a := CatalogNames()
	b := CatalogNames()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("CatalogNames() not stable at %d: %q != %q", i, a[i], b[i])
		}
	}
}

func TestAnyLiveOAT(t *testing.T) {
	now := time.Now()
	revoked := &now
	if anyLiveOAT(nil) {
		t.Error("nil OATs should not be live")
	}
	if anyLiveOAT([]authcore.OrgAccessToken{{RevokedAt: revoked}}) {
		t.Error("only-revoked OATs should not count as live")
	}
	if !anyLiveOAT([]authcore.OrgAccessToken{{RevokedAt: nil}}) {
		t.Error("a non-revoked OAT should count as live")
	}
	if !anyLiveOAT([]authcore.OrgAccessToken{{RevokedAt: revoked}, {RevokedAt: nil}}) {
		t.Error("a mix with one live OAT should count as live")
	}
}

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

// TestCreditsSpendPermission_GateSemantics proves the billing:spend (#246) gate:
// the operator role holds credits:spend by default (member role default-grant),
// an OAT WITHOUT it fails the spend gate while still passing credits:write, and
// PermAdmin satisfies it.
func TestCreditsSpendPermission_GateSemantics(t *testing.T) {
	// Default-grant: the operator role includes billing:spend.
	if !contains(OperatorRolePermissions(), PermCreditsSpend) {
		t.Fatalf("operator role must include %q (default member grant, #246)", PermCreditsSpend)
	}

	// A cost-governed OAT with write but NOT spend fails the spend gate.
	writeOnly := &ResolvedOAT{Permissions: []string{PermCreditsWrite}}
	if writeOnly.HasPermission(PermCreditsSpend) {
		t.Fatalf("credits:write must NOT imply credits:spend (#246: separate gates)")
	}
	if !writeOnly.HasPermission(PermCreditsWrite) {
		t.Fatalf("write-only OAT must still pass the write gate")
	}

	// An OAT holding billing:spend passes the spend gate.
	spender := &ResolvedOAT{Permissions: []string{PermCreditsWrite, PermCreditsSpend}}
	if !spender.HasPermission(PermCreditsSpend) {
		t.Fatalf("OAT holding %q must pass the spend gate", PermCreditsSpend)
	}

	// PermAdmin satisfies the spend gate (broad operator authority).
	admin := &ResolvedOAT{Permissions: []string{PermAdmin}}
	if !admin.HasPermission(PermCreditsSpend) {
		t.Fatalf("PermAdmin must satisfy the spend gate")
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
