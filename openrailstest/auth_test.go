package openrailstest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	helpersauthtest "github.com/open-rails/helpers/auth/authtest"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/openrailstest"
)

// perm is a host's permission, as AuthKit's iam.Perm is.
type perm string

func (p perm) String() string { return string(p) }

const (
	read  perm = "root:customers:read"
	write perm = "root:customers:update"
)

const (
	customerID = "11111111-1111-4111-8111-111111111111"
	staffID    = "22222222-2222-4222-8222-222222222222"
	staleID    = "33333333-3333-4333-8333-333333333333"
)

func routesWith(a openrails.Authenticator) openrails.Routes {
	return openrails.Routes{Auth: a, Scope: authtest.Scope, RouteGroups: openrails.RouteGroups{Admin: true, Programmatic: true},
		Permissions: openrails.Permissions{AdminRead: read, AdminUpdate: write}}
}

func request(token string) func() *http.Request {
	return func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/billing/v1/admin/payments", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return r
	}
}

// recorder captures CheckAuth's failures instead of failing the test.
type recorder struct {
	testing.TB
	failures []string
}

func (r *recorder) Errorf(format string, args ...any) { r.failures = append(r.failures, format) }
func (r *recorder) Fatal(args ...any)                 { r.failures = append(r.failures, "fatal") }
func (r *recorder) Fatalf(format string, args ...any) { r.failures = append(r.failures, format) }
func (r *recorder) Helper()                           {}

func cases(fake *authtest.Fake) helpersauthtest.Cases {
	staff := fake.Person(staffID, read.String(), write.String())
	return helpersauthtest.Cases{
		Staff: request(staff),
		User:  request(fake.Person(customerID)),
		Holders: map[string]func() *http.Request{
			read.String():  request(fake.Person("44444444-4444-4444-8444-444444444444", read.String())),
			write.String(): request(fake.Person("55555555-5555-4555-8555-555555555555", write.String())),
		},
		Stale:       request(fake.Issue(authtest.Grant{Identity: authtest.User(staleID), Permissions: []string{read.String(), write.String()}, Stale: true})),
		Application: request(fake.Machine("key_1", read.String(), write.String())),
		Refused:     map[string]func() *http.Request{"forged": request("test_forged"), "malformed": request("not-a-token")},
		Revoke:      func() { fake.Revoke(staff) },
	}
}

func TestCheckAuthPassesAConformingAuthenticator(t *testing.T) {
	fake := &authtest.Fake{}
	openrailstest.CheckAuth(t, routesWith(fake), cases(fake))
}

// The checks fail an Authenticator that breaks the contract the gates rely
// on, or the mount's own Scope and Permissions.
func TestCheckAuthCatchesABrokenAuthenticator(t *testing.T) {
	fake := &authtest.Fake{}
	for name, tc := range map[string]struct {
		routes openrails.Routes
		cases  func(helpersauthtest.Cases) helpersauthtest.Cases
	}{
		"a pass-through Authenticator":  {routesWith(authtest.PassThrough{}), nil},
		"a Can ignoring the permission": {routesWith(anyPermission{fake}), nil},
		"a person as the application": {routesWith(fake), func(c helpersauthtest.Cases) helpersauthtest.Cases {
			c.Application = request(fake.Person("66666666-6666-4666-8666-666666666666", read.String(), write.String()))
			return c
		}},
		"no application with Programmatic": {routesWith(fake), func(c helpersauthtest.Cases) helpersauthtest.Cases {
			c.Application = nil
			return c
		}},
		"an opaque staff subject": {routesWith(fake), func(c helpersauthtest.Cases) helpersauthtest.Cases {
			c.Staff = request(fake.Person("staff-1", read.String(), write.String()))
			return c
		}},
		"another scope than the mount's": {routesWith(fake), func(c helpersauthtest.Cases) helpersauthtest.Cases {
			c.Scope = billingauth.Scope{Authority: "test", ID: "other"}
			return c
		}},
	} {
		c := cases(fake)
		if tc.cases != nil {
			c = tc.cases(c)
		}
		r := &recorder{TB: t}
		openrailstest.CheckAuth(r, tc.routes, c)
		if len(r.failures) == 0 {
			t.Errorf("%s passed", name)
		}
	}
}

// anyPermission grants whoever holds any permission at all.
type anyPermission struct{ *authtest.Fake }

func (a anyPermission) Authenticate(r *http.Request) (billingauth.Verified, error) {
	v, err := a.Fake.Authenticate(r)
	if err != nil {
		return nil, err
	}
	return anyGrant{v.(authtest.Verified)}, nil
}

type anyGrant struct{ authtest.Verified }

func (v anyGrant) Can(ctx context.Context, scope billingauth.Scope, _ string) (bool, error) {
	for _, p := range v.Grant.Permissions {
		if ok, err := v.Verified.Can(ctx, scope, p); ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}
