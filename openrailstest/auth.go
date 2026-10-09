// Package openrailstest checks a host's integration in the host's own CI.
package openrailstest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/billingauth"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
)

// AuthCases are the requests CheckAuth drives through an Auth. Each is a
// function returning a fresh request, since a credential's proof (DPoP) may
// be spent once.
type AuthCases struct {
	// Permission is a merchant permission Staff holds on the mounted merchant
	// and Customer does not.
	Permission string
	// Customer is a signed-in user who is not staff.
	Customer func() *http.Request
	// Staff is a person holding Permission on the mounted merchant, signed in
	// recently.
	Staff func() *http.Request
	// Refused are credentials Required must refuse, by name: an expired
	// token, a forged one, a banned or deleted user's, a signed-out session.
	Refused map[string]func() *http.Request
	// OtherMerchantStaff holds Permission only on another merchant; nil
	// skips the case.
	OtherMerchantStaff func() *http.Request
	// StaleStaff holds Permission but signed in too long ago to move money;
	// nil skips the case.
	StaleStaff func() *http.Request
	// Machine is a machine credential (an API key) holding Permission; nil
	// skips the case. RequirePermission must admit it, and it must never
	// read as a user acting in person, who would buy as a customer.
	Machine func() *http.Request
}

// CheckAuth fails t when a's middleware admits what it must refuse:
// anonymous and refused credentials, a customer or another merchant's staff
// on a merchant permission, a stale sign-in on an operation that moves money,
// or a machine reading as a user. It also fails when a refuses the customer
// or staff it must admit, so a check cannot pass vacuously.
func CheckAuth(t testing.TB, a openrails.Auth, c AuthCases) {
	t.Helper()
	if a == nil || c.Permission == "" || c.Customer == nil || c.Staff == nil {
		t.Fatal("openrailstest: CheckAuth needs an Auth, a Permission, and Customer and Staff requests")
	}
	required := chain(a.Required())
	permission := chain(a.RequirePermission(c.Permission))
	sensitive := chain(a.RequirePermission(c.Permission), a.Sensitive())

	anonymous := func() *http.Request {
		r := c.Customer()
		r.Header.Del("Authorization")
		r.Header.Del("Cookie")
		r.Header.Del("DPoP")
		return r
	}
	refuses(t, "Required admitted an anonymous request", required, anonymous)
	refuses(t, "RequirePermission admitted an anonymous request", permission, anonymous)
	for name, req := range c.Refused {
		refuses(t, fmt.Sprintf("Required admitted the %s credential", name), required, req)
		refuses(t, fmt.Sprintf("RequirePermission admitted the %s credential", name), permission, req)
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
	refuses(t, "RequirePermission admitted the customer, who does not hold "+c.Permission, permission, c.Customer)

	admits(t, "Required refused the staff member", a, required, c.Staff)
	admits(t, "RequirePermission refused the staff member holding "+c.Permission, a, permission, c.Staff)
	admits(t, "Sensitive refused the staff member, recently signed in", a, sensitive, c.Staff)

	if c.OtherMerchantStaff != nil {
		refuses(t, "RequirePermission admitted another merchant's staff", permission, c.OtherMerchantStaff)
	}
	if c.StaleStaff != nil {
		refuses(t, "Sensitive admitted a stale sign-in", sensitive, c.StaleStaff)
	}
	if c.Machine != nil {
		if who, ok := admits(t, "RequirePermission refused the machine credential holding "+c.Permission, a, permission, c.Machine); ok && billingauth.Interactive(who) {
			t.Errorf("openrailstest: the machine credential's Identity reads as a user in person (%+v): it would buy as a customer", who.Credential)
		}
		if who, ok := admitted(a, required, c.Machine); ok && billingauth.Interactive(who) {
			t.Errorf("openrailstest: the machine credential's Identity reads as a user in person (%+v): it would buy as a customer", who.Credential)
		}
	}
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
