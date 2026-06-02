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
		"openrails:entitlements:read",
		"openrails:catalog:write",
		"openrails:payments:refund",
		"openrails:subscriptions:cancel",
		// Self-service (browser-direct) permissions (#222 browser tier).
		"openrails:self:billing:read",
		"openrails:self:checkout:create",
		"openrails:self:subscriptions:cancel",
		"openrails:self:payment-methods:manage",
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

func TestOperatorRolePermissions_IsFullCatalog(t *testing.T) {
	perms := OperatorRolePermissions()
	names := CatalogNames()
	if len(perms) != len(names) {
		t.Fatalf("operator role perms = %d, catalog = %d", len(perms), len(names))
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

func TestToGinPath(t *testing.T) {
	cases := map[string]string{
		"/token":                         "/token",
		"/owners/{slug}":                 "/owners/:slug",
		"/user/sessions/{id}":            "/user/sessions/:id",
		"/orgs/{org}/members/{user_id}":  "/orgs/:org/members/:user_id",
		"/user/providers/{provider}":     "/user/providers/:provider",
		"":                               "",
	}
	for in, want := range cases {
		if got := toGinPath(in); got != want {
			t.Errorf("toGinPath(%q) = %q, want %q", in, got, want)
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
