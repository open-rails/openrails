// Package openrailstest checks a host's integration in the host's own CI.
package openrailstest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/open-rails/openrails/internal/billingauth"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
)

// AuthCases are the requests CheckAuth drives through an Auth. Each is a
// function returning a fresh request, since a credential's proof (DPoP) may
// be spent once.
type AuthCases struct {
	// Permissions are the host's mount (Routes.Permissions).
	Permissions openrails.Permissions
	// Customer is a signed-in user who is not staff.
	Customer func() *http.Request
	// Staff is a person holding every one of Permissions on the mounted
	// merchant, signed in recently.
	Staff func() *http.Request
	// Holders are people each holding only one permission, keyed by it (its
	// String()): CheckAuth checks it admits them and every other permission
	// refuses them. Nil skips the case.
	Holders map[string]func() *http.Request
	// Refused are credentials Required must refuse, by name: an expired
	// token, a forged one, a banned or deleted user's, a signed-out session.
	Refused map[string]func() *http.Request
	// OtherMerchantStaff holds every permission only on another merchant;
	// nil skips the case.
	OtherMerchantStaff func() *http.Request
	// StaleStaff holds every permission but signed in too long ago to move
	// money; nil skips the case.
	StaleStaff func() *http.Request
	// Machine is a machine credential (an API key) holding every
	// permission; nil skips the case. RequirePermission must admit it, and it
	// must never read as a user acting in person, who would buy as a customer.
	Machine func() *http.Request
}

// CheckAuth fails t when a's middleware admits what it must refuse:
// anonymous and refused credentials, a customer or another merchant's staff
// on a permission, one permission's holder on another, a stale sign-in on an
// operation that moves money, or a machine reading as a user. It also fails
// when a refuses the customer or staff it must admit, so a check cannot pass
// vacuously.
func CheckAuth(t testing.TB, a openrails.Auth, c AuthCases) {
	t.Helper()
	perms := map[string]bool{}
	for _, perm := range []fmt.Stringer{c.Permissions.AdminRead, c.Permissions.AdminWrite, c.Permissions.CatalogWrite, c.Permissions.MerchantConfig} {
		if perm != nil && perm.String() != "" {
			perms[perm.String()] = true
		}
	}
	if a == nil || len(perms) == 0 || c.Customer == nil || c.Staff == nil {
		t.Fatal("openrailstest: CheckAuth needs an Auth, Permissions, and Customer and Staff requests")
		return
	}
	required := chain(a.Required())
	anonymous := func() *http.Request {
		r := c.Customer()
		r.Header.Del("Authorization")
		r.Header.Del("Cookie")
		r.Header.Del("DPoP")
		return r
	}
	refuses(t, "Required admitted an anonymous request", required, anonymous)
	for name, req := range c.Refused {
		refuses(t, fmt.Sprintf("Required admitted the %s credential", name), required, req)
	}
	if who, ok := admits(t, "Required refused the customer", a, required, c.Customer); ok {
		if who.SubjectKind != openrails.SubjectUser {
			t.Errorf("openrailstest: the customer's Identity is SubjectKind %q; a customer is a user", who.SubjectKind)
		}
		if who.Invoker != (openrails.Invoker{Issuer: who.Issuer, ID: who.Subject}) {
			t.Errorf("openrailstest: the customer's Identity Invoker is %+v; a subject acting itself is its own invoker", who.Invoker)
		}
		if who.Issuer == "" || who.Credential.Kind == "" {
			t.Errorf("openrailstest: the customer's Identity names no Issuer or Credential kind")
		}
		if id, err := uuid.Parse(who.Subject); err != nil || id == uuid.Nil || id.String() != who.Subject {
			t.Errorf("openrailstest: the customer's Identity Subject %q is not a canonical UUID", who.Subject)
		}
	}
	admits(t, "Required refused the staff member", a, required, c.Staff)

	for _, perm := range sortedKeys(perms) {
		permission := chain(a.RequirePermission(perm))
		sensitive := chain(a.RequirePermission(perm), a.Sensitive())
		refuses(t, "RequirePermission("+perm+") admitted an anonymous request", permission, anonymous)
		for name, req := range c.Refused {
			refuses(t, fmt.Sprintf("RequirePermission(%s) admitted the %s credential", perm, name), permission, req)
		}
		refuses(t, "RequirePermission admitted the customer, who does not hold "+perm, permission, c.Customer)
		admits(t, "RequirePermission refused the staff member holding "+perm, a, permission, c.Staff)
		admits(t, "Sensitive refused the staff member holding "+perm+", recently signed in", a, sensitive, c.Staff)
		if c.OtherMerchantStaff != nil {
			refuses(t, "RequirePermission("+perm+") admitted another merchant's staff", permission, c.OtherMerchantStaff)
		}
		if c.StaleStaff != nil {
			refuses(t, "Sensitive admitted a stale sign-in holding "+perm, sensitive, c.StaleStaff)
		}
		if c.Machine != nil {
			if who, ok := admits(t, "RequirePermission refused the machine credential holding "+perm, a, permission, c.Machine); ok && billingauth.Interactive(who) {
				t.Errorf("openrailstest: the machine credential's Identity reads as a user in person (%+v): it would buy as a customer", who.Credential)
			}
		}
	}
	if c.Machine != nil {
		if who, ok := admitted(a, required, c.Machine); ok && billingauth.Interactive(who) {
			t.Errorf("openrailstest: the machine credential's Identity reads as a user in person (%+v): it would buy as a customer", who.Credential)
		}
	}
	for own, holder := range c.Holders {
		if !perms[own] {
			t.Errorf("openrailstest: Holders names %s, which Permissions does not", own)
			continue
		}
		admits(t, fmt.Sprintf("RequirePermission(%s) refused its holder", own), a, chain(a.RequirePermission(own)), holder)
		for _, perm := range sortedKeys(perms) {
			if perm != own {
				refuses(t, fmt.Sprintf("RequirePermission(%s) admitted a holder of only %s", perm, own), chain(a.RequirePermission(perm)), holder)
			}
		}
	}
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func chain(mw ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		for i := len(mw) - 1; i >= 0; i-- {
			if mw[i] == nil {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
			}
			next = mw[i](next)
		}
		return next
	}
}

// admitted runs req through mw and reports the Identity it admitted.
func admitted(a openrails.Auth, mw func(http.Handler) http.Handler, req func() *http.Request) (openrails.Identity, bool) {
	var who openrails.Identity
	var reached, ok bool
	mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		who, ok = a.Identity(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), req())
	return who, reached && ok
}

func admits(t testing.TB, msg string, a openrails.Auth, mw func(http.Handler) http.Handler, req func() *http.Request) (openrails.Identity, bool) {
	t.Helper()
	who, ok := admitted(a, mw, req)
	if !ok {
		t.Errorf("openrailstest: %s (or Identity found no one after it)", msg)
	}
	return who, ok
}

func refuses(t testing.TB, msg string, mw func(http.Handler) http.Handler, req func() *http.Request) {
	t.Helper()
	reached := false
	w := httptest.NewRecorder()
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })).ServeHTTP(w, req())
	switch {
	case reached:
		t.Errorf("openrailstest: %s", msg)
	case w.Code < 400:
		t.Errorf("openrailstest: a refusal answered %d, not an error status (%s)", w.Code, msg)
	}
}
