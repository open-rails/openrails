// Package billingauth is OpenRails' auth contract: the host's Auth
// (helpers/auth), the identities the route gate binds, its refusals, and the
// cookie admission every mount shares. It imports no auth provider.
package billingauth

import (
	"context"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
)

// Authenticator is the host's auth (helpers/auth): it says who a request is.
// OpenRails builds every gate from the Verified it returns, asking its Can
// for a permission in a Scope and its CheckRecentSignIn for a person's recent
// sign-in, and answers every refusal itself.
type Authenticator = auth.Authenticator

// Verified is one request's verified credential (helpers/auth).
type Verified = auth.Verified

// Scope is where a permission is held (helpers/auth): a group of an issuer.
type Scope = auth.Scope

// Identity is who the Authenticator says a request is (helpers/auth):
// Subject, the native account whose authority and money are used; Invoker,
// who actually acts (the subject itself, or a party acting on its behalf);
// Credential, how it was proven. Balances and authority are the subject's, limits key on the
// invoker, and audit records all three.
type Identity = auth.Identity

// SubjectKind is a user or an application.
type SubjectKind = auth.SubjectKind

const (
	SubjectUser        = auth.SubjectUser
	SubjectApplication = auth.SubjectApplication
)

// Invoker is the party acting for an Identity's Subject.
type Invoker = auth.Invoker

// SelfActing reports a subject acting itself: its invoker is itself.
func SelfActing(who Identity) bool { return who.SelfInvoked() }

// Credential is the credential a Subject presented.
type Credential = auth.Credential

// CredentialKind is how a Subject proved itself.
type CredentialKind = auth.CredentialKind

const (
	CredentialSession     = auth.CredentialSession
	CredentialDeviceKey   = auth.CredentialDeviceKey
	CredentialAPIKey      = auth.CredentialAPIKey
	CredentialSignedToken = auth.CredentialSignedToken
	CredentialAccessToken = auth.CredentialAccessToken
)

// Interactive reports a user acting in person: not an application, and not
// a key or signed token automating the account. Only an interactive subject
// starts a payment for itself.
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

// HostScope is where the in-process host holds every permission: its own
// merchant.
func HostScope(mid billing.MerchantID) Scope {
	return Scope{Authority: HostIssuer, ID: mid.String()}
}

// Errors the internal Authenticators classify with errors.Is.
var (
	ErrUnauthenticated = auth.ErrUnauthenticated
	ErrForbidden       = auth.ErrForbidden
)

// Staff is a merchant route's admitted identity, with the route it was
// admitted for and the merchant it acts on. Limits and audit key on
// (Issuer, Subject).
type Staff struct {
	Identity
	// Route is the key of the route the gate admitted it for.
	Route    string
	Merchant billing.MerchantID
}

type identityKey struct{}
type staffKey struct{}
type merchantKey struct{}
type verifiedKey struct{}

// BindVerified records the request's Verified: the route gate's later steps
// and the handlers that offer other routes' actions ask its Can, and nothing
// verifies the request again or keeps it past the request. Only the route
// gate calls it.
func BindVerified(ctx context.Context, v Verified) context.Context {
	return context.WithValue(ctx, verifiedKey{}, v)
}

// VerifiedFrom is the request's Verified the route gate bound.
func VerifiedFrom(ctx context.Context) (Verified, bool) {
	if ctx == nil {
		return nil, false
	}
	v, ok := ctx.Value(verifiedKey{}).(Verified)
	return v, ok && v != nil
}

// MerchantBinding is embedded by OpenRails' own Verified types whose
// credential names the merchant it acts on: a merchant API key, a trusted
// issuer's access token, the in-process host. Its method is unexported, so no
// host's Verified can claim a merchant.
type MerchantBinding struct{ target Target }

// BindsMerchant is a MerchantBinding naming target.
func BindsMerchant(target Target) MerchantBinding { return MerchantBinding{target: target} }

func (b MerchantBinding) boundMerchant() Target { return b.target }

type merchantBound interface{ boundMerchant() Target }

// NamedMerchant is the merchant v's credential names, when it is one of
// OpenRails' own that names one.
func NamedMerchant(v Verified) (Target, bool) {
	b, ok := v.(merchantBound)
	if !ok {
		return Target{}, false
	}
	t := b.boundMerchant()
	return t, !t.MerchantID.IsZero()
}

// BindMerchant records the merchant the route gate pinned a request to.
// Only the route gate calls it.
func BindMerchant(ctx context.Context, id billing.MerchantID) context.Context {
	return context.WithValue(ctx, merchantKey{}, id)
}

// BoundMerchant is the merchant the route gate pinned the request to.
func BoundMerchant(ctx context.Context) (billing.MerchantID, bool) {
	if ctx == nil {
		return billing.MerchantID{}, false
	}
	id, ok := ctx.Value(merchantKey{}).(billing.MerchantID)
	return id, ok && !id.IsZero()
}

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
