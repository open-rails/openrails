package openrailstest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	helpersauthtest "github.com/open-rails/helpers/auth/authtest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/openrailstest"
)

// The admin group's root: permissions, which CheckAuth fills in.
const (
	read  = billing.RootBillingRead
	write = billing.RootBillingManage
)

const (
	customerID = "11111111-1111-4111-8111-111111111111"
	staffID    = "22222222-2222-4222-8222-222222222222"
	staleID    = "33333333-3333-4333-8333-333333333333"
)

func routesWith(a openrails.Authenticator) openrails.Routes {
	return openrails.Routes{Auth: a, RouteGroups: openrails.RouteGroups{Admin: true}}
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
	staff := fake.Person(staffID, read, write)
	return helpersauthtest.Cases{
		Staff: request(staff),
		User:  request(fake.Person(customerID)),
		Holders: map[string]func() *http.Request{
			string(read):  request(fake.Person("44444444-4444-4444-8444-444444444444", read)),
			string(write): request(fake.Person("55555555-5555-4555-8555-555555555555", write)),
		},
		Stale:       request(fake.Issue(authtest.Grant{Identity: authtest.User(staleID), Permissions: []billing.Permission{read, write}, Stale: true})),
		Application: request(fake.Machine("key_1", read, write)),
		Refused:     map[string]func() *http.Request{"forged": request("test_forged"), "malformed": request("not-a-token")},
		Revoke:      func() { fake.Revoke(staff) },
	}
}

func TestCheckAuthPassesAConformingAuthenticator(t *testing.T) {
	fake := &authtest.Fake{}
	openrailstest.CheckAuth(t, routesWith(fake), cases(fake))
}

// The checks fail an Authenticator that breaks the contract the gates rely
// on, or cases that are not the mount's: its groups' root: permissions, held
// in the Authenticator's root scope.
func TestCheckAuthCatchesABrokenAuthenticator(t *testing.T) {
	fake := &authtest.Fake{}
	for name, tc := range map[string]struct {
		routes openrails.Routes
		cases  func(helpersauthtest.Cases) helpersauthtest.Cases
	}{
		"a pass-through Authenticator":  {routesWith(authtest.PassThrough{}), nil},
		"a Can ignoring the permission": {routesWith(anyPermission{fake}), nil},
		"a person as the application": {routesWith(fake), func(c helpersauthtest.Cases) helpersauthtest.Cases {
			c.Application = request(fake.Person("66666666-6666-4666-8666-666666666666", read, write))
			return c
		}},
		"no application with Programmatic": {openrails.Routes{Auth: fake, RouteGroups: openrails.RouteGroups{Programmatic: true}}, func(c helpersauthtest.Cases) helpersauthtest.Cases {
			c.Application = nil
			return c
		}},
		"an opaque staff subject": {routesWith(fake), func(c helpersauthtest.Cases) helpersauthtest.Cases {
			c.Staff = request(fake.Person("staff-1", read, write))
			return c
		}},
		"a scope that is not the root scope": {routesWith(fake), func(c helpersauthtest.Cases) helpersauthtest.Cases {
			c.Scope = billingauth.Scope{Authority: "test", ID: "other"}
			return c
		}},
		"permissions that are not the route groups'": {routesWith(fake), func(c helpersauthtest.Cases) helpersauthtest.Cases {
			c.Permissions = []string{string(billing.RootCatalogManage)}
			return c
		}},
		"an Authenticator with no root scope": {routesWith(rootless{fake}), nil},
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

// rootless is an Authenticator that is no helpers/auth Root.
type rootless struct{ a *authtest.Fake }

func (r rootless) Authenticate(req *http.Request) (billingauth.Verified, error) {
	return r.a.Authenticate(req)
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
		if ok, err := v.Verified.Can(ctx, scope, string(p)); ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}

// CheckAuth asks for the root: permission of every route the groups mount on
// an embedded engine, and no more: the customer network's are the standalone
// server's.
func TestCheckAuthFillsTheRouteGroupsPermissions(t *testing.T) {
	fake := &authtest.Fake{}
	held := []billing.Permission{
		billing.RootBillingRead, billing.RootBillingManage, billing.RootCatalogManage, billing.RootConfigManage, billing.RootMetricsRead,
		billing.RootEntitlementsRead, billing.RootCatalogRead, billing.RootUsageManage, billing.RootCostsManage, billing.RootEventsRead, billing.RootOrdersManage,
	}
	every := openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}
	staff := fake.Person(staffID, held...)
	c := helpersauthtest.Cases{Staff: request(staff), User: request(fake.Person(customerID)), Application: request(fake.Machine("key_1", held...))}
	openrailstest.CheckAuth(t, openrails.Routes{Auth: fake, RouteGroups: every}, c)

	// One permission short, the staff member does not hold what the groups need.
	c.Staff = request(fake.Person(staffID, held[1:]...))
	r := &recorder{TB: t}
	openrailstest.CheckAuth(r, openrails.Routes{Auth: fake, RouteGroups: every}, c)
	require.NotEmpty(t, r.failures)
}
