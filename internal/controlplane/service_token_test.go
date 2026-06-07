package controlplane

import (
	"testing"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"

	"github.com/open-rails/openrails/pkg/tenant"
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
			err := validateServiceTokenResources(tenant.DefaultID, []authcore.ServiceTokenResource{
				TenantResource(tenant.DefaultID),
				{Kind: kind, ID: "legacy"},
			})
			if err != ErrServiceTokenScopeDenied {
				t.Fatalf("validateServiceTokenResources(%q) error = %v, want %v", kind, err, ErrServiceTokenScopeDenied)
			}
		})
	}
}

func TestServiceJWTPermissionIntersection(t *testing.T) {
	got := intersectPermissions(
		[]string{PermCreditsRead, PermCreditsWrite, PermCreditsSpend},
		[]string{PermCreditsRead, PermCreditsSpend},
	)
	if len(got) != 2 || got[0] != PermCreditsRead || got[1] != PermCreditsSpend {
		t.Fatalf("intersectPermissions() = %#v", got)
	}

	got = intersectPermissions([]string{PermCreditsRead}, []string{PermAdmin})
	if len(got) != 1 || got[0] != PermCreditsRead {
		t.Fatalf("admin grant should allow requested permission, got %#v", got)
	}
}

func TestServiceJWTResourceIntersection(t *testing.T) {
	subjectResource := TenantSubjectResource(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	granted := []authcore.ServiceTokenResource{
		TenantResource(tenant.DefaultID),
		subjectResource,
	}
	got, err := intersectResources(tenant.DefaultID, []authcore.ServiceTokenResource{subjectResource}, granted)
	if err != nil {
		t.Fatalf("intersectResources() error = %v", err)
	}
	if !hasResource(got, ResourceKindTenant, tenant.DefaultID.String()) {
		t.Fatalf("intersectResources() must retain tenant resource, got %#v", got)
	}
	if !hasResource(got, ResourceKindTenantSubject, subjectResource.ID) {
		t.Fatalf("intersectResources() missing requested subject resource, got %#v", got)
	}

	_, err = intersectResources(tenant.DefaultID, []authcore.ServiceTokenResource{{Kind: ResourceKindTenantSubject, ID: "other"}}, granted)
	if err != ErrServiceTokenScopeDenied {
		t.Fatalf("intersectResources() error = %v, want %v", err, ErrServiceTokenScopeDenied)
	}
}
