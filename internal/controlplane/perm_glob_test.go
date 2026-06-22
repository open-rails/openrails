package controlplane

import "testing"

// TestHasPermissionGlobUniformAcrossCredentials proves #565: every credential
// type matches permissions identically via AuthKit's namespace-anchored glob —
// a minter may put an exact perm OR a `ns:*` glob on the credential and both
// resolve the same way; a bare `*` never matches and a different namespace's
// glob never crosses over.
func TestHasPermissionGlobUniformAcrossCredentials(t *testing.T) {
	cases := []struct {
		name   string
		grants []string
		perm   string
		want   bool
	}{
		{"glob covers concrete", []string{"merchant:*"}, "merchant:catalog:update", true},
		{"glob covers other concrete", []string{"merchant:*"}, "merchant:customer-settings:read", true},
		{"exact matches exact", []string{"merchant:catalog:update"}, "merchant:catalog:update", true},
		{"exact does not cover sibling", []string{"merchant:catalog:update"}, "merchant:catalog:read", false},
		{"other namespace glob never crosses", []string{"customer:*"}, "merchant:catalog:update", false},
		{"bare wildcard never matches", []string{"*"}, "merchant:catalog:update", false},
		{"empty grants deny", nil, "merchant:catalog:update", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&ResolvedDelegated{Permissions: tc.grants}).HasPermission(tc.perm); got != tc.want {
				t.Errorf("ResolvedDelegated grants=%v perm=%q: got %v want %v", tc.grants, tc.perm, got, tc.want)
			}
			if got := (&ResolvedServiceCredential{Permissions: tc.grants}).HasPermission(tc.perm); got != tc.want {
				t.Errorf("ResolvedServiceCredential grants=%v perm=%q: got %v want %v", tc.grants, tc.perm, got, tc.want)
			}
		})
	}
}
