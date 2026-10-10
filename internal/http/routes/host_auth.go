package routes

import (
	"context"
	"net/http"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
)

// HostAuth is the in-process transport's Authenticator: the embedding host's
// own principal, a context value the transport attaches and no network
// request can carry. It is an application holding every permission at its
// own merchant, which its credential names. It admits no customer.
type HostAuth struct{}

var _ billingauth.Authenticator = HostAuth{}

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
	target, err := hostTarget(r, hp)
	if err != nil {
		return nil, err
	}
	id := hp.Subject
	if id == "" {
		id = "host"
	}
	return hostVerified{MerchantBinding: billingauth.BindsMerchant(target), id: billingauth.Identity{
		Issuer: billingauth.HostIssuer, Subject: id, SubjectKind: billingauth.SubjectApplication,
		Invoker: billingauth.Invoker{Issuer: billingauth.HostIssuer, ID: id}, Credential: billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: "in-process"},
	}}, nil
}

type hostVerified struct {
	billingauth.MerchantBinding
	id billingauth.Identity
}

func (v hostVerified) Identity() billingauth.Identity { return v.id }

// Can grants the host every permission at its own merchant.
func (v hostVerified) Can(_ context.Context, scope billingauth.Scope, permission string) (bool, error) {
	target, ok := billingauth.NamedMerchant(v)
	return ok && permission != "" && scope == billingauth.HostScope(target.MerchantID), nil
}

// CheckRecentSignIn: an application has no sign-in of its own.
func (hostVerified) CheckRecentSignIn(context.Context) error { return auth.ErrForbidden }

// hostTarget is the merchant an in-process request acts on: the host
// principal's, which a resolved selector must agree with.
func hostTarget(r *http.Request, hp *requestauth.HostPrincipal) (billingauth.Target, error) {
	target := billingauth.Target{MerchantID: hp.MerchantID, MerchantSlug: hp.MerchantSlug}
	if captured, ok := merchanttarget.FromContext(r.Context()); ok {
		if captured.MerchantID != hp.MerchantID {
			return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantBindingMismatch, "host merchant binding mismatch")
		}
		target = captured
	}
	if err := merchanttarget.Assert(r, target); err != nil {
		return billingauth.Target{}, err
	}
	return target, nil
}

// HostOptions mounts every staff and programmatic route for the in-process
// transport, whose host holds every permission at its own merchant.
func HostOptions() Options {
	return Options{
		Auth:            HostAuth{},
		ResolveMerchant: CredentialOnly,
		Scope: func(_ context.Context, mid billing.MerchantID) (billingauth.Scope, error) {
			return billingauth.HostScope(mid), nil
		},
		Permissions: hostPermissions(),
	}
}

// hostPermissions gives every Permissions field the host's own.
func hostPermissions() Permissions {
	var p Permissions
	for _, n := range AllNeeds() {
		*p.Field(n) = "host"
	}
	return p
}
