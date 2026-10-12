// Package openrailstest checks a host's integration in the host's own CI.
package openrailstest

import (
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/auth/authtest"

	"github.com/open-rails/openrails"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// CheckAuth fails t when routes.Auth breaks the contract OpenRails' gates
// rely on: AuthKit's auth/authtest.Check with the root: permissions of the route
// groups routes turns on (billing.Permissions), which it fills into c
// (Permissions c sets must be the same), held in the Authenticator's
// RootScope, so c.Scope stays zero; and OpenRails' own rule that a person's
// subject is a canonical UUID, as its customers are. With
// RouteGroups.Programmatic, c.Application is required. A mount with no
// group on gives no Permissions: c names its own, and their Scope.
func CheckAuth(t testing.TB, routes openrails.Routes, c authtest.Cases) {
	t.Helper()
	if routes.Auth == nil {
		t.Fatal("openrailstest: CheckAuth needs Routes.Auth")
		return
	}
	if perms := rootPermissions(routes.RouteGroups); len(perms) > 0 {
		if len(c.Permissions) > 0 && !sameSet(c.Permissions, perms) {
			t.Fatalf("openrailstest: Cases.Permissions %q are not the route groups' root: permissions %q", c.Permissions, perms)
			return
		}
		c.Permissions = perms
	}
	if routes.RouteGroups.Programmatic && c.Application == nil {
		t.Fatal("openrailstest: RouteGroups.Programmatic takes your backend's application: Cases.Application is required")
		return
	}
	for _, person := range []struct {
		name string
		req  func() *http.Request
	}{{"Staff", c.Staff}, {"User", c.User}} {
		if person.req == nil {
			continue
		}
		v, err := routes.Auth.Authenticate(person.req())
		if err != nil || v == nil {
			continue // authtest.Check reports it
		}
		if subject := v.Identity().Subject; !canonicalUUID(subject) {
			t.Errorf("openrailstest: %s's Identity Subject %q is not a canonical UUID; OpenRails' customers are", person.name, subject)
		}
	}
	authtest.Check(t, routes.Auth, c)
}

// rootPermissions are the root: permissions of the routes g turns on, once
// each. The customer network's routes are the standalone server's alone.
func rootPermissions(g openrails.RouteGroups) []string {
	var out []string
	for _, r := range httproutes.Catalog() {
		if !httproutes.Serves(g, r) || r.When == httproutes.FeatureCustomerNetwork {
			continue
		}
		if perm := string(r.Needs().Root); perm != "" && !slices.Contains(out, perm) {
			out = append(out, perm)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}
