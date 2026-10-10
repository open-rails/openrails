// Package authtest is an Authenticator for tests: bearer tokens it issues
// name an identity and the permissions its subject holds in Scope.
package authtest

import (
	"context"
	"crypto/rand"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/internal/billingauth"
)

// Scope is where Fake's subjects hold their permissions.
var Scope = billingauth.Scope{Authority: "test", ID: "staff"}

// Grant is what one token carries.
type Grant struct {
	Identity    billingauth.Identity
	Permissions []string
	// Stale fails CheckRecentSignIn: the sign-in is too old to move money.
	Stale bool
}

// Fake is an Authenticator over the tokens it issued. The zero value refuses
// everyone.
type Fake struct {
	mu     sync.Mutex
	grants map[string]Grant
}

var _ billingauth.Authenticator = (*Fake)(nil)

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

// Revoke ends token, as signing out does.
func (f *Fake) Revoke(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.grants, token)
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

// Authenticate admits a token Fake issued.
func (f *Fake) Authenticate(r *http.Request) (billingauth.Verified, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, auth.ErrUnauthenticated
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	g, issued := f.grants[strings.TrimSpace(token)]
	if !issued {
		return nil, auth.ErrUnauthenticated
	}
	return Verified{Grant: g}, nil
}

// Verified is a Grant as a request's Verified: it holds its permissions in
// Scope, and a person signed in recently unless Stale.
type Verified struct{ Grant Grant }

func (v Verified) Identity() billingauth.Identity { return v.Grant.Identity }

func (v Verified) Can(_ context.Context, scope billingauth.Scope, permission string) (bool, error) {
	return scope == Scope && permission != "" && slices.Contains(v.Grant.Permissions, permission), nil
}

// CheckRecentSignIn: an application, or a person's key, has no sign-in of
// its own.
func (v Verified) CheckRecentSignIn(context.Context) error {
	id := v.Grant.Identity
	switch {
	case id.SubjectKind != billingauth.SubjectUser, id.Credential.Kind == billingauth.CredentialAPIKey, id.Credential.Kind == billingauth.CredentialSignedToken:
		return auth.ErrForbidden
	case v.Grant.Stale:
		return &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: 15 * time.Minute}
	}
	return nil
}

// PassThrough is a broken Authenticator: it admits every request without
// saying who it is. OpenRails must refuse every gated route behind it.
type PassThrough struct{}

var _ billingauth.Authenticator = PassThrough{}

func (PassThrough) Authenticate(*http.Request) (billingauth.Verified, error) { return nobody{}, nil }

type nobody struct{}

func (nobody) Identity() billingauth.Identity { return billingauth.Identity{} }

// Deny refuses every request.
type Deny struct{}

var _ billingauth.Authenticator = Deny{}

func (Deny) Authenticate(*http.Request) (billingauth.Verified, error) {
	return nil, auth.ErrUnauthenticated
}
