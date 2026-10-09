package routes

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
)

// HostAuth gates the merchant routes of the in-process transport: the
// embedding host's own principal, a context value the transport attaches and
// no network request can carry. It admits no customer.
type HostAuth struct{}

var _ billingauth.Auth = HostAuth{}

func refuseWith(w http.ResponseWriter, r *http.Request, err error) {
	billingauth.WriteRefusal(w, r, billingauth.AsRefusal(err))
}

// Required admits the in-process host principal and binds its merchant.
func (HostAuth) Required() func(http.Handler) http.Handler { return admitHost }

// RequirePermission admits the host, its merchant's owner, whatever the
// permission, and binds that merchant.
func (HostAuth) RequirePermission(string) func(http.Handler) http.Handler { return admitHost }

func admitHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hp, ok := requestauth.HostPrincipalFromContext(r.Context())
		switch {
		case !ok:
			refuseWith(w, r, billingauth.ErrUnauthenticated)
			return
		case hp.MerchantID.IsZero():
			refuseWith(w, r, billingauth.Refusal(billing.CodeHostPrincipalInvalid))
			return
		}
		target, err := hostTarget(r, hp)
		if err != nil {
			refuseWith(w, r, err)
			return
		}
		ctx := merchanttarget.WithResolved(r.Context(), target)
		next.ServeHTTP(w, r.WithContext(merchant.WithID(ctx, target.MerchantID)))
	})
}

// Sensitive admits the host: it is trusted for its own merchant.
func (HostAuth) Sensitive() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return next }
}

// Identity is the in-process host: an application acting itself.
func (HostAuth) Identity(ctx context.Context) (billingauth.Identity, bool) {
	hp, ok := requestauth.HostPrincipalFromContext(ctx)
	if !ok || hp.MerchantID.IsZero() {
		return billingauth.Identity{}, false
	}
	id := hp.Subject
	if id == "" {
		id = "host"
	}
	return billingauth.Identity{
		Issuer: billingauth.HostIssuer, Subject: id, SubjectKind: billingauth.SubjectApplication,
		Invoker: billingauth.Invoker{Issuer: billingauth.HostIssuer, ID: id}, Credential: billingauth.Credential{Kind: billingauth.CredentialAPIKey, ID: "in-process"},
	}, true
}

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

// HostOptions mounts every staff route for the in-process transport, whose
// HostAuth admits the host whatever the permission.
func HostOptions() Options {
	return Options{Auth: HostAuth{}, AuthBindsMerchant: true, Permissions: Permissions{AdminRead: "host", AdminUpdate: "host", Catalog: "host", MerchantConfig: "host", Metrics: "host"}}
}
