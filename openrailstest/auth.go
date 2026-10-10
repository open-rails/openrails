// Package openrailstest checks a host's integration in the host's own CI.
package openrailstest

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
	auth "github.com/open-rails/helpers/auth"
	"github.com/open-rails/helpers/auth/authtest"

	"github.com/open-rails/openrails"
)

// CheckAuth fails t when routes.Auth breaks the contract OpenRails' gates
// rely on: helpers' authtest.Check with routes' Scope and Permissions, which
// it fills into c (a Scope or Permissions c sets must be the same), and
// OpenRails' own rule that a person's subject is a canonical UUID, as its
// customers are. With RouteGroups.Programmatic, c.Application is required.
// A mount with no staff group gives no Scope or Permissions: c names its
// own.
func CheckAuth(t testing.TB, routes openrails.Routes, c authtest.Cases) {
	t.Helper()
	if routes.Auth == nil {
		t.Fatal("openrailstest: CheckAuth needs Routes.Auth")
		return
	}
	if perms := permissions(routes.Permissions); len(perms) > 0 {
		switch {
		case c.Scope != (auth.Scope{}) && c.Scope != routes.Scope:
			t.Fatalf("openrailstest: Cases.Scope %+v is not Routes.Scope %+v", c.Scope, routes.Scope)
			return
		case len(c.Permissions) > 0 && !sameSet(c.Permissions, perms):
			t.Fatalf("openrailstest: Cases.Permissions %q are not Routes.Permissions %q", c.Permissions, perms)
			return
		}
		c.Scope, c.Permissions = routes.Scope, perms
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

// permissions are each staff group's permission, once each.
func permissions(p openrails.Permissions) []string {
	var out []string
	for _, perm := range []fmt.Stringer{p.AdminRead, p.AdminUpdate, p.Catalog, p.MerchantConfig, p.Metrics} {
		if perm == nil {
			continue
		}
		if s := perm.String(); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
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
