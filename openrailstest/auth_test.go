package openrailstest_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails"
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

var permissions = openrails.Permissions{AdminRead: read, AdminUpdate: write}

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
func (r *recorder) Helper()                           {}

func TestCheckAuthPassesAConformingAuth(t *testing.T) {
	fake := &authtest.Fake{}
	staff := fake.Person("22222222-2222-4222-8222-222222222222", read.String(), write.String())
	stale := fake.Issue(authtest.Grant{Identity: authtest.User("33333333-3333-4333-8333-333333333333"), Permissions: []string{read.String(), write.String()}, Stale: true})
	openrailstest.CheckAuth(t, fake, openrailstest.AuthCases{
		Permissions: permissions,
		Customer:    request(fake.Person("11111111-1111-4111-8111-111111111111")),
		Staff:       request(staff),
		Holders: map[string]func() *http.Request{
			read.String():  request(fake.Person("44444444-4444-4444-8444-444444444444", read.String())),
			write.String(): request(fake.Person("55555555-5555-4555-8555-555555555555", write.String())),
		},
		Refused:    map[string]func() *http.Request{"forged": request("test_forged"), "malformed": request("not-a-token")},
		StaleStaff: request(stale),
		Machine:    request(fake.Machine("key_1", read.String(), write.String())),
	})
	// The programmatic routes take an application with no permission at all.
	openrailstest.CheckAuth(t, fake, openrailstest.AuthCases{
		Programmatic: true,
		Customer:     request(fake.Person("11111111-1111-4111-8111-111111111111")),
		Staff:        request(fake.Person("22222222-2222-4222-8222-222222222222", read.String())),
		Machine:      request(fake.Machine("svc_1")),
	})
}

// A person passed as the backend fails, and so does staff an Auth reports as
// an application: the programmatic routes admit by subject kind alone.
func TestCheckAuthCatchesASubjectKindMixUp(t *testing.T) {
	fake := &authtest.Fake{}
	for name, cases := range map[string]openrailstest.AuthCases{
		"a person as the application": {
			Programmatic: true,
			Customer:     request(fake.Person("11111111-1111-4111-8111-111111111111")),
			Staff:        request(fake.Person("22222222-2222-4222-8222-222222222222", read.String())),
			Machine:      request(fake.Person("33333333-3333-4333-8333-333333333333")),
		},
		"staff as an application": {
			Permissions: openrails.Permissions{AdminRead: read},
			Customer:    request(fake.Person("11111111-1111-4111-8111-111111111111")),
			Staff:       request(fake.Machine("svc_2", read.String())),
		},
	} {
		r := &recorder{TB: t}
		openrailstest.CheckAuth(r, fake, cases)
		if len(r.failures) == 0 {
			t.Fatalf("%s passed", name)
		}
	}
}

func TestCheckAuthCatchesAPassThroughAuth(t *testing.T) {
	r := &recorder{TB: t}
	openrailstest.CheckAuth(r, authtest.PassThrough{}, openrailstest.AuthCases{
		Permissions: permissions,
		Customer:    request("anyone"),
		Staff:       request("anyone"),
		Refused:     map[string]func() *http.Request{"forged": request("forged")},
	})
	if len(r.failures) < 4 {
		t.Fatalf("a pass-through Auth drew %d failures: %v", len(r.failures), r.failures)
	}
}

// An Auth that admits staff for any permission they hold, not the one asked,
// fails the holders' check.
func TestCheckAuthCatchesAnAuthIgnoringThePermission(t *testing.T) {
	fake := &authtest.Fake{}
	r := &recorder{TB: t}
	openrailstest.CheckAuth(r, anyPermission{fake}, openrailstest.AuthCases{
		Permissions: permissions,
		Customer:    request(fake.Person("11111111-1111-4111-8111-111111111111")),
		Staff:       request(fake.Person("22222222-2222-4222-8222-222222222222", read.String(), write.String())),
		Holders: map[string]func() *http.Request{
			read.String(): request(fake.Person("44444444-4444-4444-8444-444444444444", read.String())),
		},
	})
	if len(r.failures) == 0 {
		t.Fatal("an Auth ignoring the permission passed")
	}
}

// anyPermission admits whoever holds any permission at all.
type anyPermission struct{ *authtest.Fake }

func (a anyPermission) RequirePermission(string) func(http.Handler) http.Handler {
	return a.Fake.RequirePermission(read.String())
}
