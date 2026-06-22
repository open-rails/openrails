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
		PermMerchantCustomerSettingsRead,
		PermMerchantCustomerSettingsUpdate,
		PermMerchantPaymentsRead,
		PermMerchantPaymentsRefund,
		PermMerchantSubscriptionsRead,
		PermMerchantSubscriptionsUpdate,
		PermMerchantAdmissionsCreate,
		PermMerchantUsageRead,
		PermMerchantRepairAlertsRead,
		PermCustomerBalanceRead,
		PermCustomerBillingUpdate,
		PermCustomerPaymentMethodsUpdate,
		PermCustomerCheckoutCreate,
		PermCustomerSpendDelegationsRead,
		PermCustomerSpendDelegationsUpdate,
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
		"merchant:customers:read",
		"merchant:customers:update",
	}
	names := map[string]bool{}
	for _, name := range CatalogNames() {
		names[name] = true
	}
	for _, name := range old {
		if names[name] {
			t.Fatalf("old OpenRails org permission %q must not be in the #554 catalog", name)
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

func TestMerchantOwnerRolePermissions_AreMerchantCatalog(t *testing.T) {
	// #567: the merchant owner role resolves to the `merchant:*` subset of the
	// catalog (the customer treasury perms belong to the customer persona, not the
	// merchant owner).
	perms := MerchantOwnerRolePermissions()
	if len(perms) == 0 {
		t.Fatal("merchant owner role perms must be non-empty")
	}
	catalog := map[string]bool{}
	for _, n := range CatalogNames() {
		catalog[n] = true
	}
	for _, p := range perms {
		if !strings.HasPrefix(p, MerchantType+":") {
			t.Errorf("merchant owner perm %q must be in the merchant: namespace", p)
		}
		if !catalog[p] {
			t.Errorf("merchant owner perm %q is not a catalog permission", p)
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

func TestMerchantOwnerRolePermissions_ExcludePlatformAndApexGrants(t *testing.T) {
	for _, p := range MerchantOwnerRolePermissions() {
		if strings.HasPrefix(p, "platform:") {
			t.Fatalf("merchant owner role must NOT include platform permission %q", p)
		}
	}
	// A foreign-persona apex glob must never appear in the concrete catalog.
	if contains(MerchantOwnerRolePermissions(), "root:*") {
		t.Fatalf("merchant owner role must not include apex grant %q", "root:*")
	}
}

// TestAdmissionCreatePermission_GateSemantics proves the admission hot-path gate:
// the operator role holds merchant:admissions:create by default, and an API key
// WITHOUT it fails the admission gate while still passing customer writes.
func TestAdmissionCreatePermission_GateSemantics(t *testing.T) {
	// Default-grant: the merchant owner role includes merchant:admissions:create.
	if !contains(MerchantOwnerRolePermissions(), PermMerchantAdmissionsCreate) {
		t.Fatalf("merchant owner role must include %q (default owner grant, #246)", PermMerchantAdmissionsCreate)
	}

	// A customer-write key without admission authority fails the spend gate.
	writeOnly := &ResolvedServiceCredential{Permissions: []string{PermMerchantCustomerSettingsUpdate}}
	if writeOnly.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("merchant:customer-settings:update must NOT imply merchant:admissions:create")
	}
	if !writeOnly.HasPermission(PermMerchantCustomerSettingsUpdate) {
		t.Fatalf("write-only API key must still pass the write gate")
	}

	// An API key holding merchant:admissions:create passes the spend gate.
	spender := &ResolvedServiceCredential{Permissions: []string{PermMerchantCustomerSettingsUpdate, PermMerchantAdmissionsCreate}}
	if !spender.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("API key holding %q must pass the spend gate", PermMerchantAdmissionsCreate)
	}

	admin := &ResolvedServiceCredential{Permissions: []string{"root:*"}}
	if admin.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("foreign-persona apex grant %q must not bypass merchant spend gate", "root:*")
	}
}

func TestCatalogNames_ExcludePlatformPermissions(t *testing.T) {
	for _, p := range CatalogNames() {
		if strings.HasPrefix(p, "platform:") {
			t.Fatalf("OpenRails app catalog must not include platform permission %q", p)
		}
	}
}

// TestCatalogPermissionsCoveredByOwnerGrant guards every catalog permission against
// a persona-owner lockout under the permission-group model (#567).
//
// Each type's `owner` role auto-holds `<type>:*` (authkit), so a merchant owner
// holds `merchant:*` and a customer owner holds `customer:*` — owner-grant
// coverage is namespace-anchored. The ONE namespace no type owner can ever hold
// is the separate `platform:` layer. So the surviving invariant is: every catalog
// permission must be namespaced and must NOT be `platform:`, or its persona owner
// cannot exercise it. This catches a `platform:` leak or an unnamespaced perm.
func TestCatalogPermissionsCoveredByOwnerGrant(t *testing.T) {
	const platformNS = "platform"
	for _, name := range CatalogNames() {
		ns := strings.SplitN(name, ":", 2)[0]
		if ns == "" || ns == name {
			t.Fatalf("catalog permission %q has no namespace; owner-grant coverage is namespace-anchored (#554/#100)", name)
		}
		if ns == platformNS {
			t.Fatalf("catalog permission %q is in the %q: layer, which no org owner can ever hold; "+
				"OpenRails org-scoped perms must be app namespaces like merchant: or customer: (#554/#100)", name, platformNS)
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
