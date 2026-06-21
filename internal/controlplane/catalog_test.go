package controlplane

import (
	"strings"
	"testing"
	"time"

	authcore "github.com/open-rails/authkit/core"
)

func TestCatalog_ContainsRequiredPermissions(t *testing.T) {
	// Issue #224 fixed permission catalog. If this list changes, the seeding and
	// the operator-role grant must be reviewed together.
	want := []string{
		PermMerchantSettingsRead,
		PermMerchantSettingsUpdate,
		PermMerchantPaymentProvidersRead,
		PermMerchantPaymentProvidersUpdate,
		PermMerchantCatalogRead,
		PermMerchantCatalogUpdate,
		PermMerchantCustomersRead,
		PermMerchantCustomersUpdate,
		PermMerchantPaymentsRead,
		PermMerchantPaymentsRefund,
		PermMerchantSubscriptionsRead,
		PermMerchantSubscriptionsUpdate,
		PermMerchantAdmissionsCreate,
		PermMerchantUsageRead,
		PermMerchantRepairAlertsRead,
		PermOrgSpendDelegationsRead,
		PermOrgSpendDelegationsUpdate,
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

func TestCatalog_HardCutsOldOrgMerchantPermissions(t *testing.T) {
	old := []string{
		"org:credits:read",
		"org:credits:update",
		"org:credits:spend",
		"org:billing:read",
		"org:billing:update",
		"org:checkout:create",
		"org:subscriptions:read",
		"org:subscriptions:update",
		"org:payment-methods:read",
		"org:payment-methods:update",
		"org:entitlements:read",
		"org:entitlements:update",
		"org:product_access:update",
		"org:catalog:update",
		"org:secrets:read",
		"org:configuration:read",
		"org:metrics:read",
	}
	names := map[string]bool{}
	for _, name := range CatalogNames() {
		names[name] = true
	}
	for _, name := range old {
		if names[name] {
			t.Fatalf("old OpenRails org permission %q must not be in the #554 catalog", name)
		}
		if IsDelegatedPermission(name) {
			t.Fatalf("old OpenRails org permission %q must not satisfy delegated permission checks", name)
		}
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

func TestOperatorRolePermissions_AreOrgOnly(t *testing.T) {
	for _, p := range OperatorRolePermissions() {
		if strings.HasPrefix(p, "platform:") {
			t.Fatalf("operator role must NOT include platform permission %q", p)
		}
	}
	if contains(OperatorRolePermissions(), authcore.OrgOwnerGrant) {
		t.Fatalf("operator role must not include apex grant %q", authcore.OrgOwnerGrant)
	}
}

// TestAdmissionCreatePermission_GateSemantics proves the admission hot-path gate:
// the operator role holds merchant:admissions:create by default, and an API key
// WITHOUT it fails the admission gate while still passing customer writes.
func TestAdmissionCreatePermission_GateSemantics(t *testing.T) {
	// Default-grant: the operator role includes merchant:admissions:create.
	if !contains(OperatorRolePermissions(), PermMerchantAdmissionsCreate) {
		t.Fatalf("operator role must include %q (default member grant, #246)", PermMerchantAdmissionsCreate)
	}

	// A customer-write key without admission authority fails the spend gate.
	writeOnly := &ResolvedServiceCredential{Permissions: []string{PermMerchantCustomersUpdate}}
	if writeOnly.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("merchant:customers:update must NOT imply merchant:admissions:create")
	}
	if !writeOnly.HasPermission(PermMerchantCustomersUpdate) {
		t.Fatalf("write-only API key must still pass the write gate")
	}

	// An API key holding merchant:admissions:create passes the spend gate.
	spender := &ResolvedServiceCredential{Permissions: []string{PermMerchantCustomersUpdate, PermMerchantAdmissionsCreate}}
	if !spender.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("API key holding %q must pass the spend gate", PermMerchantAdmissionsCreate)
	}

	admin := &ResolvedServiceCredential{Permissions: []string{authcore.OrgOwnerGrant}}
	if admin.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("apex grant %q must not bypass exact spend gate", authcore.OrgOwnerGrant)
	}
}

func TestCatalogNames_AreOrgOnly(t *testing.T) {
	for _, p := range CatalogNames() {
		if strings.HasPrefix(p, "platform:") {
			t.Fatalf("org catalog must not include platform permission %q", p)
		}
	}
}

// TestCatalogPermissionsCoveredByOwnerGrant guards every catalog permission against
// a merchant-owner lockout — correct both before and after the #554 `merchant:*`
// rename.
//
// The owner-coverage fix now exists (AuthKit #100): OpenRails sets
// authcore OwnerOwnsAppResources (see internal/controlplane/service.go), so the
// bootstrapped org owner's apex grant covers `org:*` PLUS one `<ns>:*` glob for
// every app namespace OpenRails declares (e.g. `merchant:*`) — owning the org owns
// the merchant (org⇄merchant 1:1). The ONE namespace an org owner can never hold is
// the separate `platform:` layer. So the surviving invariant is: every catalog
// permission must be namespaced and must NOT be `platform:`, or a merchant owner
// cannot exercise it. This passes today (all `org:`) and keeps passing after the
// rename to `merchant:`, while still catching a `platform:` leak or an unnamespaced
// perm. (#554/#100)
func TestCatalogPermissionsCoveredByOwnerGrant(t *testing.T) {
	platformNS := strings.SplitN(authcore.PlatformSuperAdminGrant, ":", 2)[0] // "platform"
	for _, name := range CatalogNames() {
		ns := strings.SplitN(name, ":", 2)[0]
		if ns == "" || ns == name {
			t.Fatalf("catalog permission %q has no namespace; owner-grant coverage is namespace-anchored (#554/#100)", name)
		}
		if ns == platformNS {
			t.Fatalf("catalog permission %q is in the %q: layer, which no org owner can ever hold; "+
				"OpenRails org-scoped perms must be org: or an app namespace like merchant: (#554/#100)", name, platformNS)
		}
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
