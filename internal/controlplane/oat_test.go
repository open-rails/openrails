package controlplane

import "testing"

func TestResolvedOAT_HasPermission(t *testing.T) {
	r := &ResolvedOAT{Permissions: []string{PermCreditsRead, PermCreditsWrite}}

	if !r.HasPermission(PermCreditsWrite) {
		t.Errorf("expected granted permission %q to be held", PermCreditsWrite)
	}
	if r.HasPermission(PermCatalogWrite) {
		t.Errorf("did not expect ungranted permission %q to be held", PermCatalogWrite)
	}
}

func TestResolvedOAT_AdminSatisfiesAnyPermission(t *testing.T) {
	r := &ResolvedOAT{Permissions: []string{PermAdmin}}
	for _, p := range CatalogNames() {
		if !r.HasPermission(p) {
			t.Errorf("admin OAT should satisfy %q", p)
		}
	}
}

func TestResolvedOAT_EmptyDeniesAll(t *testing.T) {
	r := &ResolvedOAT{}
	if r.HasPermission(PermCreditsRead) {
		t.Error("empty OAT should grant nothing")
	}
}

func TestControlPlane_TokenPrefix_NilSafe(t *testing.T) {
	var c *ControlPlane
	if got := c.TokenPrefix(); got != "" {
		t.Errorf("nil control plane TokenPrefix() = %q, want empty", got)
	}
	if c.LooksLikeOAT("anything") {
		t.Error("nil control plane should not match OATs")
	}
}
