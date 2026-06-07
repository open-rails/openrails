package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/open-rails/openrails/pkg/authprovider"
)

// fakeAdminChecker is a test double for the live AdminPermissionChecker.
type fakeAdminChecker struct {
	// allow maps "tenant|user|perm" -> true.
	allow map[string]bool
	err   error
}

func (f fakeAdminChecker) HasAdminPermission(_ context.Context, tenantSlug, userID, perm string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.allow[tenantSlug+"|"+userID+"|"+perm], nil
}

// TestIsLiveAdmin_NilCheckerDenies proves the hardcut: without a live control
// plane there is no admin authority — IsLiveAdmin returns false and never panics
// on a nil checker. There is no operator-tenant or global-admin DB fallback.
func TestIsLiveAdmin_NilCheckerDenies(t *testing.T) {
	got, err := IsLiveAdmin(context.Background(), nil, authprovider.UserContext{UserID: "u1", Tenant: "acme"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got {
		t.Error("nil checker: admin must be denied")
	}
}

// TestIsLiveAdmin_LivePermission covers the soft helper that admin read paths
// (catalog.go) use to gate inactive-row visibility. Admin authority is the LIVE
// openrails:admin permission held in the caller's OWN tenant (#312) — not a
// claim-based operator tenant nor a JWT role.
func TestIsLiveAdmin_LivePermission(t *testing.T) {
	checker := fakeAdminChecker{allow: map[string]bool{
		"acme|u1|" + PermAdmin: true,
	}}

	cases := []struct {
		name string
		uc   authprovider.UserContext
		want bool
	}{
		{name: "no tenant -> false", uc: authprovider.UserContext{UserID: "u1"}, want: false},
		{name: "no user -> false", uc: authprovider.UserContext{Tenant: "acme"}, want: false},
		{name: "different tenant -> false", uc: authprovider.UserContext{UserID: "u1", Tenant: "globex"}, want: false},
		{name: "own tenant, holds admin -> true", uc: authprovider.UserContext{UserID: "u1", Tenant: "acme"}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IsLiveAdmin(context.Background(), checker, tc.uc)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

// TestIsLiveAdmin_PropagatesError ensures a checker error surfaces to the caller
// (the soft read paths treat any non-nil error as "not admin" but must not
// swallow it silently at this layer).
func TestIsLiveAdmin_PropagatesError(t *testing.T) {
	checker := fakeAdminChecker{err: errors.New("authkit down")}
	got, err := IsLiveAdmin(context.Background(), checker, authprovider.UserContext{UserID: "u1", Tenant: "acme"})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if got {
		t.Error("error case must not report admin")
	}
}
