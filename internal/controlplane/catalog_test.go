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
		PermCreditsRead,
		PermCreditsWrite,
		PermCreditsSpend,
		PermEntitlementsRead,
		PermCatalogWrite,
		PermPaymentsRefund,
		PermSubscriptionsCancel,
		PermMerchantBillingRead,
		PermMerchantEntitlementsWrite,
		PermMerchantProductAccessWrite,
		PermMerchantPaymentsWrite,
		PermMerchantConfigurationRead,
		PermMerchantConfigurationWrite,
		PermMerchantSecretsList,
		PermMerchantSecretsWrite,
		PermMerchantSecretsDelete,
		PermMerchantSecretsTest,
		PermMerchantMetricsRead,
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

func TestOperatorRolePermissions_AreOrgCatalog(t *testing.T) {
	perms := OperatorRolePermissions()
	names := CatalogNames()
	if len(perms) != len(names) {
		t.Fatalf("operator role perms = %d, want catalog = %d", len(perms), len(names))
	}
	set := map[string]bool{}
	for _, p := range perms {
		set[p] = true
	}
	if set[PermPlatformSuperadmin] {
		t.Fatalf("operator role must NOT include %q", PermPlatformSuperadmin)
	}
	for _, n := range names {
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

func TestAnyLiveAPIKey(t *testing.T) {
	now := time.Now()
	revoked := &now
	if anyLiveAPIKey(nil) {
		t.Error("nil API keys should not be live")
	}
	if anyLiveAPIKey([]authcore.APIKey{{RevokedAt: revoked}}) {
		t.Error("only-revoked API keys should not count as live")
	}
	if !anyLiveAPIKey([]authcore.APIKey{{RevokedAt: nil}}) {
		t.Error("a non-revoked API key should count as live")
	}
	if !anyLiveAPIKey([]authcore.APIKey{{RevokedAt: revoked}, {RevokedAt: nil}}) {
		t.Error("a mix with one live API key should count as live")
	}
}

// TestOperatorRoleExcludesPlatformSuperadmin proves the layer separation: a
// merchant operator role is granted only org-layer catalog permissions.
func TestOperatorRoleExcludesPlatformSuperadmin(t *testing.T) {
	for _, p := range OperatorRolePermissions() {
		if p == PermPlatformSuperadmin {
			t.Fatalf("operator role must NOT include %q", PermPlatformSuperadmin)
		}
	}
	if contains(OperatorRolePermissions(), PermAdmin) {
		t.Fatalf("operator role must not include apex grant %q", PermAdmin)
	}
}

// TestCreditsSpendPermission_GateSemantics proves the billing:spend (#246) gate:
// the operator role holds credits:spend by default (member role default-grant),
// and an API key WITHOUT it fails the spend gate while still passing credits:write.
func TestCreditsSpendPermission_GateSemantics(t *testing.T) {
	// Default-grant: the operator role includes billing:spend.
	if !contains(OperatorRolePermissions(), PermCreditsSpend) {
		t.Fatalf("operator role must include %q (default member grant, #246)", PermCreditsSpend)
	}

	// A cost-governed API key with write but NOT spend fails the spend gate.
	writeOnly := &ResolvedServiceCredential{Permissions: []string{PermCreditsWrite}}
	if writeOnly.HasPermission(PermCreditsSpend) {
		t.Fatalf("credits:write must NOT imply credits:spend (#246: separate gates)")
	}
	if !writeOnly.HasPermission(PermCreditsWrite) {
		t.Fatalf("write-only API key must still pass the write gate")
	}

	// An API key holding billing:spend passes the spend gate.
	spender := &ResolvedServiceCredential{Permissions: []string{PermCreditsWrite, PermCreditsSpend}}
	if !spender.HasPermission(PermCreditsSpend) {
		t.Fatalf("API key holding %q must pass the spend gate", PermCreditsSpend)
	}

	admin := &ResolvedServiceCredential{Permissions: []string{PermAdmin}}
	if admin.HasPermission(PermCreditsSpend) {
		t.Fatalf("apex grant %q must not bypass exact spend gate", PermAdmin)
	}
}

// TestPlatformRoleHoldsSuperadminGlob proves the platform role uses AuthKit's
// platform-layer apex grant.
func TestPlatformRoleHoldsOnlySuperadmin(t *testing.T) {
	perms := PlatformRolePermissions()
	if len(perms) != 1 || perms[0] != authcore.PlatformSuperAdminGrant {
		t.Fatalf("platform role must hold only %q, got %v", authcore.PlatformSuperAdminGrant, perms)
	}
}

// TestSuperadminPermNotInOrgCatalog proves platform authority is outside the
// OpenRails org permission catalog.
func TestSuperadminPermInCatalog(t *testing.T) {
	if contains(CatalogNames(), PermPlatformSuperadmin) {
		t.Fatalf("org catalog must not include %q", PermPlatformSuperadmin)
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
