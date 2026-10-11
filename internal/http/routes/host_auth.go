package routes

import (
	"context"
	"net/http"

	auth "github.com/open-rails/authkit/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/requestauth"
)

// HostAuth is the in-process transport's Authenticator: the embedding host's
// own principal, a context value the transport attaches and no network
// request can carry. It is an application bound to the merchant of its
// handle, holding every permission there and in its root scope. It admits no
// customer.
type HostAuth struct{}

var (
	_ billingauth.Authenticator = HostAuth{}
	_ auth.Root                 = HostAuth{}
)

// hostRoot is where the host holds its root: permissions.
var hostRoot = billingauth.Scope{Authority: billingauth.HostIssuer, ID: "root"}

// RootScope is the host's own root scope (authkit/auth Root).
func (HostAuth) RootScope(context.Context) (billingauth.Scope, error) { return hostRoot, nil }

// Authenticate admits the in-process host principal.
func (HostAuth) Authenticate(r *http.Request) (billingauth.Verified, error) {
	if r == nil {
		return nil, billingauth.ErrUnauthenticated
	}
	hp, ok := requestauth.HostPrincipalFromContext(r.Context())
	switch {
	case !ok:
		return nil, billingauth.ErrUnauthenticated
	case hp.MerchantID.IsZero():
		return nil, billingauth.Refusal(billing.CodeHostPrincipalInvalid)
	}
	id := hp.Subject
	if id == "" {
		id = "host"
	}
	return hostVerified{MerchantBinding: billingauth.BindsMerchant(billingauth.Target{MerchantID: hp.MerchantID}), id: billingauth.Identity{
		Issuer: billingauth.HostIssuer, Subject: id, SubjectKind: billingauth.SubjectApplication,
		Invoker: billingauth.Invoker{Issuer: billingauth.HostIssuer, ID: id}, Credential: billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: "in-process"},
	}}, nil
}

type hostVerified struct {
	billingauth.MerchantBinding
	id billingauth.Identity
}

func (v hostVerified) Identity() billingauth.Identity { return v.id }

// Can grants the host every permission at its own merchant and in its root
// scope.
func (v hostVerified) Can(_ context.Context, scope billingauth.Scope, permission string) (bool, error) {
	target, ok := billingauth.NamedMerchant(v)
	return ok && permission != "" && (scope == billingauth.HostScope(target.MerchantID) || scope == hostRoot), nil
}

// CheckRecentSignIn: an application has no sign-in of its own.
func (hostVerified) CheckRecentSignIn(context.Context) error { return auth.ErrForbidden }

// HostOptions mounts every staff and programmatic route for the in-process
// transport, whose host holds every permission at its own merchant.
func HostOptions() Options {
	return Options{
		Auth:   HostAuth{},
		Groups: config.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true},
	}
}
