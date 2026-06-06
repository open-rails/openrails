package controlplane

import "testing"

func TestResolvedServiceToken_HasPermission(t *testing.T) {
	r := &ResolvedServiceToken{Permissions: []string{PermCreditsRead, PermCreditsWrite}}

	if !r.HasPermission(PermCreditsWrite) {
		t.Errorf("expected granted permission %q to be held", PermCreditsWrite)
	}
	if r.HasPermission(PermCatalogWrite) {
		t.Errorf("did not expect ungranted permission %q to be held", PermCatalogWrite)
	}
}

func TestResolvedServiceToken_AdminSatisfiesAnyPermission(t *testing.T) {
	r := &ResolvedServiceToken{Permissions: []string{PermAdmin}}
	for _, p := range CatalogNames() {
		if !r.HasPermission(p) {
			t.Errorf("admin service token should satisfy %q", p)
		}
	}
}

func TestResolvedServiceToken_EmptyDeniesAll(t *testing.T) {
	r := &ResolvedServiceToken{}
	if r.HasPermission(PermCreditsRead) {
		t.Error("empty service token should grant nothing")
	}
}

func TestControlPlane_TokenPrefix_NilSafe(t *testing.T) {
	var c *ControlPlane
	if got := c.TokenPrefix(); got != "" {
		t.Errorf("nil control plane TokenPrefix() = %q, want empty", got)
	}
	if c.LooksLikeServiceToken("anything") {
		t.Error("nil control plane should not match service tokens")
	}
}
