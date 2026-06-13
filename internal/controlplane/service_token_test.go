package controlplane

import (
	"testing"

	authcore "github.com/open-rails/authkit/core"

	"github.com/open-rails/openrails/internal/dbtest"
)

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

func TestValidateServiceTokenResourcesRejectsLegacyPayableKinds(t *testing.T) {
	legacyKinds := []string{
		"tenant_subject_id",
		"payer_account_id",
		"account_id",
		"delegated_user_id",
		"subject_type",
		"openrails.payer_account",
		"openrails.account",
		"openrails.delegated_user",
	}
	for _, kind := range legacyKinds {
		t.Run(kind, func(t *testing.T) {
			err := validateServiceTokenResources(dbtest.TestTenantID, []authcore.ServiceTokenResource{
				TenantResource(dbtest.TestTenantID),
				{Kind: kind, ID: "legacy"},
			})
			if err != ErrServiceTokenScopeDenied {
				t.Fatalf("validateServiceTokenResources(%q) error = %v, want %v", kind, err, ErrServiceTokenScopeDenied)
			}
		})
	}
}

// Service-JWT authority is no longer an intersection of a requested permission
// set against a server-side grant. Registering an issuer to a tenant grants that
// tenant full authority over its own resources; the self-signed token's claims
// are authoritative, bounded only by validateServiceTokenResources (own-tenant
// scope). The former intersectPermissions/intersectResources tests were removed
// with that logic.
