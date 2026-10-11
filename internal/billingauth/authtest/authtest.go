// Package authtest is an Authenticator for tests: bearer tokens it issues
// name an identity and the permissions its subject holds in the root scope,
// or in one merchant's.
package authtest

import (
	"context"
	"crypto/rand"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	auth "github.com/open-rails/authkit/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
)

// Issuer is the fakes' issuer and the authority of their scopes. A host
// whose merchant declares it as its Issuer acts there with their tokens.
const Issuer = "https://issuer.test"

// Root is the fakes' root scope: where a Grant holds its permissions unless
// it names a Scope.
var Root = billingauth.Scope{Authority: Issuer, ID: "root"}

// MerchantScope is where mid's own staff hold their permissions.
func MerchantScope(mid billing.MerchantID) billingauth.Scope {
	return billingauth.Scope{Authority: Root.Authority, ID: mid.String()}
}

// Grant is what one token carries.
type Grant struct {
	Identity    billingauth.Identity
	Permissions []billing.Permission
	// Scope is where Permissions are held; zero is Root.
	Scope billingauth.Scope
	// Bound is the scope the credential is bound to (authkit/auth Bound),
	// such as a merchant's group; zero binds none. Bound, it holds nothing
	// elsewhere but a person's root permissions in Root.
	Bound billingauth.Scope
	// Stale fails CheckRecentSignIn: the sign-in is too old to move money.
	Stale bool
}

// Fake is an Authenticator over the tokens it issued. The zero value refuses
// everyone.
type Fake struct {
	mu     sync.Mutex
	grants map[string]Grant
}

var (
	_ billingauth.Authenticator = (*Fake)(nil)
	_ auth.Root                 = (*Fake)(nil)
)

// RootScope is Root (authkit/auth Root).
func (*Fake) RootScope(context.Context) (billingauth.Scope, error) { return Root, nil }

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

// User is a user acting itself in a session, issued by Issuer.
func User(id string) billingauth.Identity {
	return billingauth.Identity{Issuer: Issuer, Subject: id, SubjectKind: billingauth.SubjectUser, Invoker: billingauth.Invoker{Issuer: Issuer, ID: id}, Credential: billingauth.Credential{Kind: billingauth.CredentialSession, ID: "s_" + id}}
}

// Application is an application acting itself with an API key.
func Application(id string) billingauth.Identity {
	return billingauth.Identity{Issuer: Issuer, Subject: id, SubjectKind: billingauth.SubjectApplication, Invoker: billingauth.Invoker{Issuer: Issuer, ID: id}, Credential: billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: "k_" + id}}
}

// Person issues a token for a user with perms in Root.
func (f *Fake) Person(id string, perms ...billing.Permission) string {
	return f.Issue(Grant{Identity: User(id), Permissions: perms})
}

// Machine issues a token for an API key with perms in Root.
func (f *Fake) Machine(id string, perms ...billing.Permission) string {
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
// its scope, and a person signed in recently unless Stale.
type Verified struct{ Grant Grant }

func (v Verified) Identity() billingauth.Identity { return v.Grant.Identity }

// BoundScope is Grant.Bound (authkit/auth Bound).
func (v Verified) BoundScope() billingauth.Scope { return v.Grant.Bound }

func (v Verified) Can(_ context.Context, scope billingauth.Scope, permission string) (bool, error) {
	if b := v.Grant.Bound; b != (billingauth.Scope{}) && scope != b && (scope != Root || v.Grant.Identity.SubjectKind != billingauth.SubjectUser) {
		return false, nil
	}
	held := v.Grant.Scope
	if held == (billingauth.Scope{}) {
		held = Root
	}
	return scope == held && permission != "" && slices.Contains(v.Grant.Permissions, billing.Permission(permission)), nil
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
func (PassThrough) RootScope(context.Context) (billingauth.Scope, error)     { return Root, nil }

type nobody struct{}

func (nobody) Identity() billingauth.Identity { return billingauth.Identity{} }

// Deny refuses every request.
type Deny struct{}

var _ billingauth.Authenticator = Deny{}

func (Deny) Authenticate(*http.Request) (billingauth.Verified, error) {
	return nil, auth.ErrUnauthenticated
}

func (Deny) RootScope(context.Context) (billingauth.Scope, error) { return Root, nil }
