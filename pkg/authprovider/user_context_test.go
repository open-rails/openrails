package authprovider

import "testing"

func TestUserContext_HasAnyMerchantRole(t *testing.T) {
	cases := []struct {
		name string
		uc   UserContext
		want []string
		ok   bool
	}{
		{
			name: "empty merchant returns false even if roles match",
			uc:   UserContext{MerchantRoles: []string{"admin"}},
			want: []string{"admin"},
			ok:   false,
		},
		{
			name: "empty want returns false",
			uc:   UserContext{Merchant: "acme", MerchantRoles: []string{"admin"}},
			want: []string{},
			ok:   false,
		},
		{
			name: "single matching role",
			uc:   UserContext{Merchant: "acme", MerchantRoles: []string{"admin"}},
			want: []string{"admin"},
			ok:   true,
		},
		{
			name: "case-insensitive role match",
			uc:   UserContext{Merchant: "acme", MerchantRoles: []string{"ADMIN"}},
			want: []string{"admin"},
			ok:   true,
		},
		{
			name: "matches against any element in want list",
			uc:   UserContext{Merchant: "acme", MerchantRoles: []string{"owner"}},
			want: []string{"admin", "owner", "billing_admin"},
			ok:   true,
		},
		{
			name: "no overlap returns false",
			uc:   UserContext{Merchant: "acme", MerchantRoles: []string{"member", "viewer"}},
			want: []string{"admin", "owner"},
			ok:   false,
		},
		{
			name: "multiple MerchantRoles, first matches",
			uc:   UserContext{Merchant: "acme", MerchantRoles: []string{"admin", "billing_admin"}},
			want: []string{"admin"},
			ok:   true,
		},
		{
			name: "multiple MerchantRoles, last matches",
			uc:   UserContext{Merchant: "acme", MerchantRoles: []string{"member", "admin"}},
			want: []string{"admin"},
			ok:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.uc.HasAnyMerchantRole(tc.want...); got != tc.ok {
				t.Errorf("got %v want %v", got, tc.ok)
			}
		})
	}
}

func TestUserContext_HasRole_Unchanged(t *testing.T) {
	// Sanity: adding Merchant/MerchantRoles fields didn't break existing HasRole behavior.
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
