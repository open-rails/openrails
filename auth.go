package openrails

import (
	"github.com/open-rails/authkit/userinfo"

	"github.com/open-rails/openrails/internal/billingauth"
)

type (
	// UserInfo is Deps.UserInfo: authkit/userinfo's Lookup, the host's
	// directory read in process (AuthKit's Client.UserInfo()). Email notices,
	// the admin customer read, lists and search ask it.
	UserInfo = userinfo.Lookup

	// Authenticator is Routes.Auth: authkit/auth's Authenticator, which says
	// who a request is (AuthKit's ak.Authenticator()). OpenRails builds its
	// gates from the Verified it returns.
	Authenticator = billingauth.Authenticator
	// Identity is who the Authenticator says a request is (authkit/auth):
	// the Subject whose authority and money is used, the Invoker acting for
	// it when that is someone else, and the Credential it was proven with.
	Identity = billingauth.Identity
	// SubjectKind is Identity.SubjectKind: SubjectUser or SubjectApplication.
	SubjectKind = billingauth.SubjectKind
	// Invoker is Identity.Invoker: the party acting for the subject.
	Invoker = billingauth.Invoker
	// Credential is Identity.Credential: how Subject proved itself.
	Credential = billingauth.Credential
	// CredentialKind is Credential.Kind.
	CredentialKind = billingauth.CredentialKind
)

const (
	// SubjectUser is a user: a customer, or staff.
	SubjectUser = billingauth.SubjectUser
	// SubjectApplication is an application account.
	SubjectApplication = billingauth.SubjectApplication

	// CredentialSession is a signed-in session's token.
	CredentialSession = billingauth.CredentialSession
	// CredentialDeviceKey is a user's enrolled device key.
	CredentialDeviceKey = billingauth.CredentialDeviceKey
	// CredentialAPIKey is an API key of the subject's.
	CredentialAPIKey = billingauth.CredentialAPIKey
	// CredentialSignedToken is a token an application signed with its own
	// key.
	CredentialSignedToken = billingauth.CredentialSignedToken
	// CredentialAccessToken is an authorization server's access token.
	CredentialAccessToken = billingauth.CredentialAccessToken
)

// ErrUnauthenticated refuses a request without a valid credential: the
// authkit/auth sentinel.
var ErrUnauthenticated = billingauth.ErrUnauthenticated
