package billingauth

import (
	"context"
	"net/http"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
)

// Auth is the host's auth, as plain net/http middleware in AuthKit's shape.
// OpenRails stacks it on each of its own routes by the route's catalog tier:
// Required on a customer route; RequirePermission on a merchant route, then
// Sensitive on one that moves money or removes access when the subject is a
// user acting in person (automation has no sign-in to renew). Refusals are
// the middleware's own responses. After them OpenRails reads Identity and
// refuses a request it finds no identity on.
type Auth interface {
	// Required admits a signed-in request and refuses anyone else. A
	// provider may admit people only; an application subject then cannot
	// reach the invoker-scoped customer routes.
	Required() func(http.Handler) http.Handler
	// RequirePermission authenticates the request itself and admits a
	// subject holding exactly permission (one merchant permission, never a
	// glob) on the mounted merchant, checked live.
	RequirePermission(permission string) func(http.Handler) http.Handler
	// Sensitive, stacked after RequirePermission, admits a request whose
	// sign-in is recent enough to move money or remove access, by the
	// provider's own policy.
	Sensitive() func(http.Handler) http.Handler
	// Identity is who the middleware admitted on ctx. On a customer route
	// the subject is the customer (a canonical UUID) and the invoker who
	// acts for it.
	Identity(ctx context.Context) (Identity, bool)
}

// Identity is who Auth admitted, mirroring helpers/auth's provider-neutral
// Identity so it can become an alias of it. Subject is whose authority and
// money is used; Invoker who actually acts (the subject itself, or a party
// acting on its behalf); Credential how it was proven.
type Identity struct {
	// Issuer is the authority that vouches for Subject. Balances and
	// authority are the subject's; limits key on the invoker; audit records
	// both, and the credential.
	Issuer string
	// Subject is a native account: a user, the same whatever credential they
	// signed in with, or an application.
	Subject     string
	SubjectKind SubjectKind
	// Invoker is the party actually acting: {Issuer, Subject} when the
	// subject acts itself, else the party acting on its behalf, possibly
	// another issuer's user spending the subject's balance. Always set.
	Invoker Invoker
	// Credential is how Subject proved itself.
	Credential Credential
	// Email, Username and EmailVerified are display and prefill only.
	Email         string
	Username      string
	EmailVerified bool
}

// SubjectKind is a user or an application.
type SubjectKind string

const (
	SubjectUser        SubjectKind = "user"
	SubjectApplication SubjectKind = "application"
)

// Invoker is the party acting for an Identity's Subject.
type Invoker struct {
	Issuer string
	ID     string
}

// SelfActing reports a subject acting itself: its invoker is itself.
func SelfActing(who Identity) bool {
	return who.Invoker == Invoker{Issuer: who.Issuer, ID: who.Subject}
}

// Credential is the credential a Subject presented.
type Credential struct {
	Kind CredentialKind
	// ID names that session, key or token, for audit.
	ID string
}

// CredentialKind is how a Subject proved itself.
type CredentialKind string

const (
	CredentialSession     CredentialKind = "session"
	CredentialDeviceKey   CredentialKind = "device_key"
	CredentialAPIKey      CredentialKind = "api_key"
	CredentialSignedToken CredentialKind = "signed_token"
	CredentialAccessToken CredentialKind = "access_token"
)

// Interactive reports a user acting in person: not an application, and not
// a key or signed token automating the account. Only an interactive subject
// starts a payment for itself; only a non-interactive one may hold a
// machine-only permission.
func Interactive(who Identity) bool {
	return who.SubjectKind == SubjectUser && SelfActing(who) &&
		who.Credential.Kind != CredentialAPIKey && who.Credential.Kind != CredentialSignedToken
}

// InvokerKey is the invoker as limits key it: the subject's own id when it
// acts itself, else the invoker's id, qualified as issuer|id when another
// issuer vouches for it.
func InvokerKey(who Identity) string {
	if SelfActing(who) {
		return who.Subject
	}
	if who.Invoker.Issuer == who.Issuer {
		return who.Invoker.ID
	}
	return who.Invoker.Issuer + "|" + who.Invoker.ID
}

// CredentialName is the credential as audit records it: kind:id.
func CredentialName(who Identity) string {
	switch {
	case who.Credential.Kind == "":
		return ""
	case who.Credential.ID == "":
		return string(who.Credential.Kind)
	}
	return string(who.Credential.Kind) + ":" + who.Credential.ID
}

// HostIssuer is the issuer of the in-process host's identity: the embedding
// host's own Go client.
const HostIssuer = "openrails:host"

// Errors the internal Auth implementations classify with errors.Is.
var (
	ErrUnauthenticated = auth.ErrUnauthenticated
	ErrForbidden       = auth.ErrForbidden
)

// Staff is a merchant route's admitted identity, with the permission it was
// admitted for and the merchant it acts on. Limits and audit key on
// (Issuer, Subject).
type Staff struct {
	Identity
	Permission string
	Merchant   billing.MerchantID
}

type identityKey struct{}
type staffKey struct{}

// BindIdentity records a customer route's admitted identity in OpenRails' own
// context. Only the route gate calls it (TestBindersHaveOneSite).
func BindIdentity(ctx context.Context, c Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, c)
}

// BindStaff records a merchant route's admitted identity. Only the route gate
// calls it.
func BindStaff(ctx context.Context, s Staff) context.Context {
	return context.WithValue(ctx, staffKey{}, s)
}

// IdentityFromContext is a customer route's admitted identity.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	c, ok := ctx.Value(identityKey{}).(Identity)
	return c, ok && c.Subject != ""
}

// StaffFromContext is a merchant route's admitted identity.
func StaffFromContext(ctx context.Context) (Staff, bool) {
	if ctx == nil {
		return Staff{}, false
	}
	s, ok := ctx.Value(staffKey{}).(Staff)
	return s, ok && s.Subject != "" && !s.Merchant.IsZero()
}
