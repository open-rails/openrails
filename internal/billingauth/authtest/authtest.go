// Package authtest is an Auth for tests: bearer tokens it issues name an
// identity and the merchant permissions its subject holds.
package authtest

import (
	"context"
	"crypto/rand"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
)

// Grant is what one token carries.
type Grant struct {
	Identity    billingauth.Identity
	Permissions []string
	// Stale fails Sensitive: the sign-in is too old to move money.
	Stale bool
}

// Fake is an Auth over the tokens it issued. The zero value refuses
// everyone.
type Fake struct {
	mu     sync.Mutex
	grants map[string]Grant
	// calls and refusals count each middleware's decisions, by name.
	calls, refusals map[string]int
}

var _ billingauth.Auth = (*Fake)(nil)

type grantKey struct{}

// Issue returns a token for g.
func (f *Fake) Issue(g Grant) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grants == nil {
		f.grants = map[string]Grant{}
	}
	token := "test_" + rand.Text()
	f.grants[token] = g
	return token
}

// User is a user acting itself in a session, issued by "test".
func User(id string) billingauth.Identity {
	return billingauth.Identity{Issuer: "test", Subject: id, SubjectKind: billingauth.SubjectUser, Invoker: billingauth.Invoker{Issuer: "test", ID: id}, Credential: billingauth.Credential{Kind: billingauth.CredentialSession, ID: "s_" + id}}
}

// Application is an application acting itself with an API key.
func Application(id string) billingauth.Identity {
	return billingauth.Identity{Issuer: "test", Subject: id, SubjectKind: billingauth.SubjectApplication, Invoker: billingauth.Invoker{Issuer: "test", ID: id}, Credential: billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: "k_" + id}}
}

// Person issues a token for a user with perms.
func (f *Fake) Person(id string, perms ...string) string {
	return f.Issue(Grant{Identity: User(id), Permissions: perms})
}

// Machine issues a token for an API key with perms.
func (f *Fake) Machine(id string, perms ...string) string {
	return f.Issue(Grant{Identity: Application(id), Permissions: perms})
}

// Admitted counts the requests a middleware admitted.
func (f *Fake) Admitted(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *Fake) admit(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
}

func refuse(w http.ResponseWriter, r *http.Request, code string) {
	billingauth.WriteRefusal(w, r, billingauth.Refusal(code))
}

// Refused counts the requests a middleware refused.
func (f *Fake) Refused(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refusals[name]
}

func (f *Fake) refuse(w http.ResponseWriter, r *http.Request, name, code string) {
	f.mu.Lock()
	if f.refusals == nil {
		f.refusals = map[string]int{}
	}
	f.refusals[name]++
	f.mu.Unlock()
	refuse(w, r, code)
}

// grant is the request's grant: the one an earlier gate admitted, else its
// bearer token's.
func (f *Fake) grant(r *http.Request) (Grant, bool) {
	if g, ok := r.Context().Value(grantKey{}).(Grant); ok {
		return g, true
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return Grant{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	g, issued := f.grants[strings.TrimSpace(token)]
	return g, issued
}

// Required admits a token Fake issued.
func (f *Fake) Required() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g, ok := f.grant(r)
			if !ok {
				f.refuse(w, r, "Required", billing.CodeAuthenticationRequired)
				return
			}
			f.admit("Required")
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey{}, g)))
		})
	}
}

// RequirePermission admits a token Fake issued whose subject holds exactly
// perm.
func (f *Fake) RequirePermission(perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g, ok := f.grant(r)
			if !ok {
				f.refuse(w, r, "RequirePermission", billing.CodeAuthenticationRequired)
				return
			}
			if !slices.Contains(g.Permissions, perm) {
				f.refuse(w, r, "RequirePermission", billing.CodePermissionRequired)
				return
			}
			f.admit("RequirePermission")
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey{}, g)))
		})
	}
}

// Sensitive admits a request whose grant is not Stale.
func (f *Fake) Sensitive() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g, ok := r.Context().Value(grantKey{}).(Grant)
			if !ok || g.Stale {
				f.refuse(w, r, "Sensitive", billing.CodeStepUpRequired)
				return
			}
			f.admit("Sensitive")
			next.ServeHTTP(w, r)
		})
	}
}

// Identity is who a gate admitted.
func (f *Fake) Identity(ctx context.Context) (billingauth.Identity, bool) {
	g, ok := ctx.Value(grantKey{}).(Grant)
	return g.Identity, ok
}

// PassThrough is a broken Auth whose middleware checks nothing and admits
// no identity: OpenRails must refuse every gated route behind it.
type PassThrough struct{}

var _ billingauth.Auth = PassThrough{}

func pass(next http.Handler) http.Handler { return next }

func (PassThrough) Required() func(http.Handler) http.Handler                { return pass }
func (PassThrough) RequirePermission(string) func(http.Handler) http.Handler { return pass }
func (PassThrough) Sensitive() func(http.Handler) http.Handler               { return pass }
func (PassThrough) Identity(context.Context) (billingauth.Identity, bool) {
	return billingauth.Identity{}, false
}

// Deny refuses every request.
type Deny struct{}

var _ billingauth.Auth = Deny{}

func deny(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { refuse(w, r, billing.CodeAuthenticationRequired) })
}

func (Deny) Required() func(http.Handler) http.Handler                { return deny }
func (Deny) RequirePermission(string) func(http.Handler) http.Handler { return deny }
func (Deny) Sensitive() func(http.Handler) http.Handler               { return deny }
func (Deny) Identity(context.Context) (billingauth.Identity, bool) {
	return billingauth.Identity{}, false
}
