package authprovider

import "testing"

func TestUserContext_HasAnyTenantRole(t *testing.T) {
	cases := []struct {
		name string
		uc   UserContext
		want []string
		ok   bool
	}{
		{
			name: "empty org returns false even if roles match",
			uc:   UserContext{TenantRoles: []string{"admin"}},
			want: []string{"admin"},
			ok:   false,
		},
		{
			name: "empty want returns false",
			uc:   UserContext{Tenant: "acme", TenantRoles: []string{"admin"}},
			want: []string{},
			ok:   false,
		},
		{
			name: "single matching role",
			uc:   UserContext{Tenant: "acme", TenantRoles: []string{"admin"}},
			want: []string{"admin"},
			ok:   true,
		},
		{
			name: "case-insensitive role match",
			uc:   UserContext{Tenant: "acme", TenantRoles: []string{"ADMIN"}},
			want: []string{"admin"},
			ok:   true,
		},
		{
			name: "matches against any element in want list",
			uc:   UserContext{Tenant: "acme", TenantRoles: []string{"owner"}},
			want: []string{"admin", "owner", "billing_admin"},
			ok:   true,
		},
		{
			name: "no overlap returns false",
			uc:   UserContext{Tenant: "acme", TenantRoles: []string{"member", "viewer"}},
			want: []string{"admin", "owner"},
			ok:   false,
		},
		{
			name: "multiple TenantRoles, first matches",
			uc:   UserContext{Tenant: "acme", TenantRoles: []string{"admin", "billing_admin"}},
			want: []string{"admin"},
			ok:   true,
		},
		{
			name: "multiple TenantRoles, last matches",
			uc:   UserContext{Tenant: "acme", TenantRoles: []string{"member", "admin"}},
			want: []string{"admin"},
			ok:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.uc.HasAnyTenantRole(tc.want...); got != tc.ok {
				t.Errorf("got %v want %v", got, tc.ok)
			}
		})
	}
}

func TestUserContext_HasRole_Unchanged(t *testing.T) {
	// Sanity: adding Tenant/TenantRoles fields didn't break existing HasRole behavior.
	uc := UserContext{Roles: []string{"admin", "moderator"}}
	if !uc.HasRole("admin") {
		t.Error("expected HasRole(admin) to be true")
	}
	if uc.HasRole("billing_admin") {
		t.Error("expected HasRole(billing_admin) to be false")
	}
	if !uc.HasRole("ADMIN") {
		t.Error("HasRole should be case-insensitive")
	}
}
